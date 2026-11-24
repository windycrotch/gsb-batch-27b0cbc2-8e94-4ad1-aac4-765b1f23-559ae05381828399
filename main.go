package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"github.com/rezmoss/citychime/internal/chime"
)

const (
	appName = "City Hall Clock"

	// shutdownTimeout bounds how long quitting may wait for playback to stop.
	shutdownTimeout = 3 * time.Second
)

func main() {
	systray.Run(app.onReady, app.onExit)
}

// The tray UI is driven from a single goroutine (the one systray runs onReady
// on). Other goroutines never touch systray directly; they only record state
// and wake the UI loop through uiWake.
var app = &application{uiWake: make(chan struct{}, 1)}

// application owns the tray UI and the lifetime of all background goroutines.
type application struct {
	scheduler *chime.Scheduler

	uiMu      sync.Mutex
	ready     bool
	stopped   bool
	status    *systray.MenuItem
	iconErr   string // set once, at startup
	audioErr  string // cleared again when a chime plays successfully
	clockCx   context.CancelFunc
	clockDone chan struct{}
	uiWake    chan struct{}
	stopOnce  sync.Once
}

func (a *application) onReady() {
	// The clock context is created first so a Quit click that races with
	// startup still cancels everything that startup goes on to launch.
	ctx, cancel := context.WithCancel(context.Background())
	a.uiMu.Lock()
	a.clockCx = cancel
	a.uiMu.Unlock()

	scheduler := chime.New(chime.Options{
		Loader:   newBeepLoader(findChimeResource),
		Reporter: a,
	})

	// A missing or unreadable icon must not prevent the tray from appearing:
	// fall back to a generated icon and report the problem instead.
	icon := fallbackIcon()
	iconErr := ""
	if fileIcon, err := loadIcon(); err != nil {
		log.Printf("%s: %v", appName, err)
		iconErr = err.Error()
	} else {
		icon = fileIcon
	}

	a.uiMu.Lock()
	a.ready = true
	a.iconErr = iconErr
	a.scheduler = scheduler
	a.status = systray.AddMenuItem("Chime ready", "Chime status")
	a.status.Disable()
	a.uiMu.Unlock()
	quit := systray.AddMenuItem("Quit", "Quit the app")

	systray.SetIcon(icon)
	systray.SetTitle(appName)
	a.refreshTray()

	// Handle Quit immediately, even if the rest of startup is still running, so
	// a click is never lost to systray's non-blocking click channel.
	uiStopped := make(chan struct{})
	go func() {
		<-quit.ClickedCh
		a.shutdown()
		close(uiStopped)
	}()

	if a.isStopped() {
		return
	}

	// Surface a missing or unreadable audio file right away instead of waiting
	// for the first quarter hour. This only looks the file up; decoding and the
	// audio device are exercised on the first chime, and both are retried on
	// every trigger.
	if _, err := findChimeResource(); err != nil {
		a.ReportError(err)
	}

	running := true
	if err := scheduler.Start(ctx); err != nil {
		if a.isStopped() {
			return // a Quit click won the race; shutting down is already done
		}
		log.Printf("%s: %v", appName, err)
		a.ReportError(err)
		running = false
	}

	if running && !a.isStopped() {
		done := make(chan struct{})
		a.uiMu.Lock()
		a.clockDone = done
		a.uiMu.Unlock()
		// RunClock returns as soon as ctx is cancelled, so it cannot linger
		// even if a Quit click arrives between here and the goroutine starting.
		go func() {
			defer close(done)
			chime.RunClock(ctx, chime.TickEvery(ctx, time.Minute), scheduler)
		}()
	}

	// UI loop: the only place tray widgets are redrawn after startup.
	for {
		select {
		case <-a.uiWake:
			a.refreshTray()
		case <-uiStopped:
			return
		}
	}
}

func (a *application) isStopped() bool {
	a.uiMu.Lock()
	defer a.uiMu.Unlock()
	return a.stopped
}

func (a *application) onExit() {
	// Backstop: normal quits go through the UI loop. This covers exits
	// triggered by the OS session ending.
	a.stopBackground()
}

// shutdown stops scheduling, aborts any chime that is playing and waits for the
// playback and clock goroutines to finish before the process is allowed to
// exit.
func (a *application) shutdown() {
	a.stopBackground()
	systray.Quit()
}

// stopBackground cancels the clock and playback and waits, within
// shutdownTimeout, for their goroutines to end.
func (a *application) stopBackground() {
	a.stopOnce.Do(func() {
		a.uiMu.Lock()
		a.stopped = true
		cancel, clockDone, scheduler := a.clockCx, a.clockDone, a.scheduler
		a.clockCx = nil
		a.uiMu.Unlock()
		if cancel != nil {
			cancel()
		}

		ctx, cancelTimeout := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelTimeout()

		if scheduler != nil {
			if err := scheduler.Shutdown(ctx); err != nil {
				log.Printf("%s: %v", appName, err)
			}
		}
		if clockDone != nil {
			select {
			case <-clockDone:
			case <-ctx.Done():
				log.Printf("%s: clock loop did not stop in time: %v", appName, ctx.Err())
			}
		}
	})
}

// ReportError implements chime.Reporter: every failure is logged and surfaced
// in the tray so the user can see it without a terminal. It is safe to call
// from the playback goroutine: the tray is redrawn by the UI loop.
func (a *application) ReportError(err error) {
	log.Printf("%s: %v", appName, err)

	a.uiMu.Lock()
	a.audioErr = err.Error()
	a.uiMu.Unlock()
	a.wakeUI()
}

// ReportHealthy implements chime.HealthReporter: once a chime has played, a
// stale failure notice (for example "audio device unavailable") is cleared so
// the tray no longer claims a problem that has healed.
func (a *application) ReportHealthy() {
	a.uiMu.Lock()
	had := a.audioErr != ""
	a.audioErr = ""
	a.uiMu.Unlock()
	if had {
		a.wakeUI()
	}
}

func (a *application) wakeUI() {
	select {
	case a.uiWake <- struct{}{}:
	default: // a redraw is already pending
	}
}

// refreshTray redraws the tooltip and status item from the current state.
func (a *application) refreshTray() {
	a.uiMu.Lock()
	ready, status, iconErr, audioErr := a.ready, a.status, a.iconErr, a.audioErr
	a.uiMu.Unlock()
	if !ready {
		return
	}

	var problems []string
	if audioErr != "" {
		problems = append(problems, audioErr)
	}
	if iconErr != "" {
		problems = append(problems, iconErr)
	}

	if len(problems) == 0 {
		systray.SetTooltip(appName)
		if status != nil {
			status.SetTitle("Chime ready")
		}
		return
	}

	notice := strings.Join(problems, "; ")
	systray.SetTooltip(appName + " — " + notice)
	if status != nil {
		status.SetTitle("Problem: " + truncate(notice, 60))
	}
}

// findChimeResource locates the chime audio next to the app bundle or the
// binary. It is called before every chime, so a missing file is not fatal: the
// failure is reported and retried, and playback resumes once the file is back.
func findChimeResource() (string, error) {
	return findResource("chime.mp3")
}

func findResource(name string) (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating executable: %w", err)
	}
	execDir := filepath.Dir(execPath)

	candidates := []string{
		filepath.Join(execDir, "..", "Resources", name),
		filepath.Join(execDir, "Resources", name),
		filepath.Join(execDir, name),
		name,
	}
	// During `go run` / `go test` the binary lives in a temp dir; the working
	// directory is then the only sensible place to look.
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, name))
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("resource %q not found (looked in %d locations)", name, len(candidates))
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
