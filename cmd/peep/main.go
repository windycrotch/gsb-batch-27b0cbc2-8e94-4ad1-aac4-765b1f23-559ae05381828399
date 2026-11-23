package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/lancekrogers/peep/internal/diagram"
	"github.com/lancekrogers/peep/internal/installation"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/lancekrogers/peep/internal/player"
	"github.com/lancekrogers/peep/internal/render"
	"github.com/lancekrogers/peep/internal/tui"
)

// Release packaging overrides this with -X main.version=<release>.
var version = "dev"

func run() error {
	if len(os.Args) == 2 && os.Args[1] == diagram.WorkerArgument {
		return diagram.Worker(context.Background(), os.Stdin, os.Stdout)
	}
	flags := flag.NewFlagSet("peep", flag.ContinueOnError)
	seekStep := flags.Duration("seek-step", 5*time.Second, "rewind/forward step (e.g. 500ms, 1s, 10s)")
	fps := flags.Int("fps", 12, "video/GIF target FPS (1–60; F changes it during playback)")
	play := flags.Bool("play", false, "start playback explicitly")
	read0 := flags.Bool("read0", false, "read NUL-separated paths from stdin (default: newline-separated)")
	doctor := flags.Bool("doctor", false, "report media dependencies")
	install := flags.Bool("install", false, "install this executable and missing media dependencies")
	prefix := flags.String("prefix", "", "installation prefix (default ~/.local)")
	binDir := flags.String("bin-dir", "", "install executable in this directory (default <prefix>/bin)")
	noDeps := flags.Bool("no-deps", false, "skip media dependency setup during --install")
	licenses := flags.Bool("licenses", false, "show embedded Mermaid license and third-party notices")
	showVersion := flags.Bool("version", false, "show version")
	renderer := flags.String("renderer", "auto", "image/video renderer: auto, kitty, or cells")
	audio := flags.String("audio-output", "", "mpv audio output override (null for automated tests)")
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), "peep — terminal file previews and media\n\nUsage: peep [flags] [directory | files...]\n       rg -l PATTERN | peep\n       rg -l -0 PATTERN | peep --read0\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *fps < 1 || *fps > 60 {
		return fmt.Errorf("--fps must be between 1 and 60")
	}
	if *seekStep <= 0 {
		return fmt.Errorf("--seek-step must be a positive duration")
	}
	if *install {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		destination := *prefix
		if destination == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			destination = filepath.Join(home, ".local")
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if !*noDeps {
			if err = installation.EnsureMedia(ctx, os.Stdin, os.Stdout, os.Stderr); err != nil {
				return err
			}
		}
		path, err := installation.InstallTo(ctx, executable, destination, *binDir)
		if err != nil {
			return err
		}
		fmt.Printf("Installed %s\n", path)
		installation.ReportPath(os.Stdout, path)
		return nil
	}
	if *licenses {
		fmt.Print(diagram.License, "\n", diagram.Notices)
		return nil
	}
	if *showVersion {
		fmt.Println("peep", buildVersion())
		return nil
	}
	mode, err := render.Renderer(*renderer)
	if err != nil {
		return err
	}
	if *doctor {
		missing := false
		if *renderer == "auto" && os.Getenv("TMUX") == "" && os.Getenv("STY") == "" {
			mode = "auto (capability probe at startup)"
		}
		fmt.Println("peep", buildVersion(), "· image/video renderer:", mode)
		for _, name := range []string{"ffmpeg", "ffprobe", "mpv"} {
			path, err := exec.LookPath(name)
			if err != nil {
				fmt.Println(name + ": MISSING")
				missing = true
			} else {
				fmt.Printf("%s: %s\n", name, path)
			}
		}
		fmt.Printf("Mermaid: built in (mmdg %s; embedded fonts)\n", diagram.Version)
		if missing {
			return fmt.Errorf("install missing media tools; see README.md")
		}
		return nil
	}
	if !term.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("peep needs an interactive terminal; use --doctor for diagnostics")
	}
	// Own catchable termination signals before starting any media children.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	input := os.Stdin
	paths := flags.Args()
	piped := !term.IsTerminal(input.Fd())
	if piped {
		if len(paths) > 0 {
			return fmt.Errorf("use either stdin paths or positional paths, not both")
		}
		stream := input
		stopRead := context.AfterFunc(ctx, func() { _ = stream.Close() })
		paths, err = readPaths(input, *read0)
		stopRead()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(paths) == 0 {
			return nil
		}
		input, err = os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open terminal for keyboard input: %w", err)
		}
		defer input.Close()
	} else if *read0 {
		return fmt.Errorf("--read0 requires a file list on stdin")
	}
	state, err := term.GetState(input.Fd())
	if err != nil {
		return fmt.Errorf("save terminal state: %w", err)
	}
	// Last resort after the renderer closes: cancel partial control strings and
	// disable modes peep owns, even if startup or the UI exits through an error.
	defer func() {
		fmt.Fprint(os.Stdout, "\x18\x1b\\\x1b[?2026l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[0m\x1b[?25h\x1b[?1049l")
		_ = term.Restore(input.Fd(), state)
	}()
	backend, err := player.New(ctx, *audio)
	notice := ""
	if err != nil {
		notice = err.Error()
	} else {
		defer backend.Close()
	}
	var model *tui.Model
	if piped {
		model, err = tui.NewResults(ctx, backend, paths, *play, notice)
	} else {
		model, err = tui.New(ctx, backend, paths, *play, notice)
	}
	if err != nil {
		return err
	}
	model.SetSeekStep(*seekStep)
	model.SetVideoFPS(*fps)
	model.SetTerminalInput(input)
	options := []tea.ProgramOption{tea.WithInput(input), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx), tea.WithoutSignalHandler()}
	var app tea.Model = model
	probe := *renderer == "auto" && os.Getenv("TMUX") == "" && os.Getenv("STY") == ""
	if mode == "kitty" || probe {
		writer := render.NewKittyWriter(os.Stdout)
		defer writer.Close()
		if probe {
			input := render.NewProbeInput(input)
			options = append(options, tea.WithInput(input))
			app = &probingModel{model: model, input: input, output: writer, ctx: ctx}
		} else {
			model.SetGraphics(writer)
		}
		options = append(options, tea.WithOutput(writer))
	}
	_, err = tea.NewProgram(app, options...).Run()
	interrupted := ctx.Err() != nil
	cancel() // Stop decoders before deferred terminal cleanup or editor teardown.
	if interrupted && errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "peep:", err)
		os.Exit(1)
	}
}
