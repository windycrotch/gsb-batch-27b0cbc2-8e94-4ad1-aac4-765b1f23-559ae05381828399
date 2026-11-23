package game

import (
	"sync"
	"testing"
	"time"

	"github.com/sauerbraten/waiter/pkg/protocol/nmc"
	"github.com/sauerbraten/waiter/pkg/protocol/playerstate"
)

// mockServer is a thread-safe game.Server implementation for clock tests.
type mockServer struct {
	mu sync.Mutex

	duration time.Duration
	players  []*Player

	broadcasts  []broadcast
	intermissns int
}

type broadcast struct {
	typ  nmc.ID
	args []any
}

func newMockServer(duration time.Duration, players ...*Player) *mockServer {
	return &mockServer{
		duration: duration,
		players:  players,
	}
}

func (s *mockServer) GameDuration() time.Duration { return s.duration }

func (s *mockServer) Broadcast(typ nmc.ID, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcasts = append(s.broadcasts, broadcast{typ: typ, args: append([]any(nil), args...)})
}

func (s *mockServer) Intermission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intermissns++
}

func (s *mockServer) ForEachPlayer(f func(*Player)) {
	s.mu.Lock()
	players := append([]*Player(nil), s.players...)
	s.mu.Unlock()
	for _, p := range players {
		f(p)
	}
}

func (s *mockServer) UniqueName(p *Player) string {
	if p == nil {
		return "server"
	}
	return p.Name
}

func (s *mockServer) NumberOfPlayers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.players {
		if p.State != playerstate.Spectator {
			n++
		}
	}
	return n
}

func (s *mockServer) countPauseBroadcasts() (on, off int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.broadcasts {
		if b.typ != nmc.PauseGame || len(b.args) < 2 {
			continue
		}
		if toggle, ok := b.args[0].(int); ok {
			if toggle == 1 {
				on++
			} else {
				off++
			}
		}
	}
	return
}

func testPlayer(cn uint32, name string) *Player {
	p := NewPlayer(cn)
	p.Name = name
	p.State = playerstate.Dead
	return &p
}

func spectator(cn uint32, name string) *Player {
	p := testPlayer(cn, name)
	p.State = playerstate.Spectator
	return p
}

// waitFor polls cond until it succeeds or the timeout elapses.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// withShortCountdown runs fn with a shortened competitive resume countdown.
func withShortCountdown(t *testing.T, fn func()) {
	t.Helper()
	original := resumeCountdown
	resumeCountdown = 60 * time.Millisecond
	defer func() { resumeCountdown = original }()
	fn()
}

func TestCasualPauseFreezesAndResumesTimeLeft(t *testing.T) {
	srv := newMockServer(time.Minute)
	clock := NewCasualClock(srv, nil)
	clock.Start()

	time.Sleep(30 * time.Millisecond)
	running := clock.TimeLeft()
	if running >= time.Minute || running <= time.Minute-100*time.Millisecond {
		t.Fatalf("timer should be counting down, got %v left", running)
	}

	p1 := testPlayer(1, "alice")
	clock.Pause(p1)
	if !clock.Paused() {
		t.Fatal("clock should be paused")
	}
	frozen := clock.TimeLeft()

	// while paused, remaining time must not move at all
	time.Sleep(60 * time.Millisecond)
	if got := clock.TimeLeft(); got != frozen {
		t.Fatalf("time left changed while paused: %v -> %v", frozen, got)
	}

	clock.Resume(p1)
	if clock.Paused() {
		t.Fatal("clock should be running after resume")
	}
	time.Sleep(30 * time.Millisecond)
	if got := clock.TimeLeft(); got >= frozen {
		t.Fatalf("timer did not continue after resume: %v (frozen at %v)", got, frozen)
	}
}

func TestCasualRepeatedPauseDoesNotStack(t *testing.T) {
	srv := newMockServer(time.Minute)
	clock := NewCasualClock(srv, nil)
	clock.Start()

	p1 := testPlayer(1, "alice")
	for i := 0; i < 3; i++ {
		clock.Pause(p1)
	}

	clock.mu.Lock()
	holders := len(clock.holders)
	clock.mu.Unlock()
	if holders != 1 {
		t.Fatalf("expected exactly 1 pause holder, got %d", holders)
	}

	on, _ := srv.countPauseBroadcasts()
	if on != 1 {
		t.Fatalf("expected exactly 1 pause broadcast, got %d", on)
	}
}

func TestCasualResumeWhileRunningDoesNotFlipState(t *testing.T) {
	srv := newMockServer(time.Minute)
	clock := NewCasualClock(srv, nil)
	clock.Start()

	p1 := testPlayer(1, "alice")
	clock.Resume(p1)
	clock.Resume(p1)

	if clock.Paused() {
		t.Fatal("clock must not become paused from resume requests")
	}
	on, off := srv.countPauseBroadcasts()
	if on != 0 || off != 0 {
		t.Fatalf("resume while running must not broadcast, got on=%d off=%d", on, off)
	}
}

func TestCasualPerPlayerHolders(t *testing.T) {
	srv := newMockServer(time.Minute)
	clock := NewCasualClock(srv, nil)
	clock.Start()

	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	clock.Pause(p1)
	clock.Pause(p2)

	clock.Resume(p1)
	if !clock.Paused() {
		t.Fatal("clock must stay paused while another player holds a pause")
	}

	clock.Resume(p2)
	if clock.Paused() {
		t.Fatal("clock must run once every holder is released")
	}
}

func TestCasualDisconnectReleasesHolders(t *testing.T) {
	srv := newMockServer(time.Minute)
	clock := NewCasualClock(srv, nil)
	clock.Start()

	p1 := testPlayer(1, "alice")
	clock.Pause(p1)
	clock.Disconnect(p1)

	if clock.Paused() {
		t.Fatal("disconnecting the pause holder must release the pause")
	}
	clock.mu.Lock()
	holders := len(clock.holders)
	clock.mu.Unlock()
	if holders != 0 {
		t.Fatalf("expected no holders after disconnect, got %d", holders)
	}
}

func TestCasualStopClearsPauseState(t *testing.T) {
	srv := newMockServer(time.Minute)
	clock := NewCasualClock(srv, nil)
	clock.Start()

	clock.Pause(testPlayer(1, "alice"))
	clock.Stop()

	if clock.Paused() {
		t.Fatal("Stop must clear pause state")
	}
}

func TestCompetitiveStartWaitsForAllSpawnsAndManualResumeCannotBypass(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		if !clock.Paused() {
			t.Fatal("game must stay paused until all players loaded the map")
		}
		frozen := clock.TimeLeft()
		time.Sleep(40 * time.Millisecond)
		if got := clock.TimeLeft(); got != frozen {
			t.Fatalf("time left changed while waiting for spawns: %v -> %v", frozen, got)
		}

		// an ordinary player's manual resume request must not unlock the spawn wait
		clock.Resume(p1)
		time.Sleep(2 * resumeCountdown)
		if !clock.Paused() {
			t.Fatal("manual resume must not bypass the waiting-for-spawns auto-pause")
		}

		clock.Spawned(p1)
		if !clock.Paused() {
			t.Fatal("game must stay paused while a player is still loading")
		}
		clock.Spawned(p2)
		if clock.Paused() {
			t.Fatal("game must run once every player spawned")
		}
	})
}

func TestCompetitiveStartWithoutPlayersRunsImmediately(t *testing.T) {
	srv := newMockServer(time.Minute)
	clock := NewCompetitiveClock(srv, nil)
	clock.Start()
	if clock.Paused() {
		t.Fatal("game with no players must not wait for spawns")
	}
}

func TestCompetitivePlayerLeaveAutoPausesAndOnlyReturnResumes(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)
		clock.Spawned(p2)
		if clock.Paused() {
			t.Fatal("game should be running after everyone spawned")
		}

		clock.Leave(p1)
		if !clock.Paused() {
			t.Fatal("leaving player must auto-pause the competitive game")
		}
		frozen := clock.TimeLeft()
		time.Sleep(40 * time.Millisecond)
		if got := clock.TimeLeft(); got != frozen {
			t.Fatalf("time left changed during auto-pause: %v -> %v", frozen, got)
		}

		// ordinary players cannot manually clear the auto-pause, not even after the countdown
		clock.Resume(p2)
		time.Sleep(2 * resumeCountdown)
		if !clock.Paused() {
			t.Fatal("manual resume must not bypass the player-left auto-pause")
		}
		if got := clock.TimeLeft(); got != frozen {
			t.Fatalf("time left changed while auto-pause was supposed to hold: %v -> %v", frozen, got)
		}

		// an eligible player coming back releases the auto-pause
		p1.State = playerstate.Dead
		clock.Join(p1)
		if clock.Paused() {
			t.Fatal("game must resume when the missing player is back")
		}
	})
}

func TestCompetitiveRepeatedPauseAndResumeDoNotStack(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)
		clock.Spawned(p2)

		for i := 0; i < 3; i++ {
			clock.Pause(p1)
		}
		clock.casualClock.mu.Lock()
		manual := clock.holderCount(manualPause)
		clock.casualClock.mu.Unlock()
		if manual != 1 {
			t.Fatalf("expected 1 manual holder, got %d", manual)
		}

		clock.Resume(p1)
		// further resume requests while the countdown is in flight must not stack
		clock.Resume(p1)
		clock.Resume(p2)
		clock.casualClock.mu.Lock()
		countdowns := len(clock.countdown)
		clock.casualClock.mu.Unlock()
		if countdowns != 3 { // one countdown sequence, 3 timers
			t.Fatalf("expected a single countdown sequence, got %d timers", countdowns)
		}

		waitFor(t, "countdown to resume", 3*resumeCountdown, func() bool {
			return !clock.Paused()
		})
	})
}

func TestCompetitivePauseAbortsInFlightResume(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)
		clock.Spawned(p2)

		clock.Pause(p1)
		clock.Resume(p1)
		clock.casualClock.mu.Lock()
		if clock.countdown == nil {
			clock.casualClock.mu.Unlock()
			t.Fatal("expected a resume countdown in flight")
		}
		clock.casualClock.mu.Unlock()

		clock.Pause(p1)
		clock.casualClock.mu.Lock()
		if clock.countdown != nil {
			clock.casualClock.mu.Unlock()
			t.Fatal("pause must abort the resume countdown")
		}
		clock.casualClock.mu.Unlock()

		time.Sleep(2 * resumeCountdown)
		if !clock.Paused() {
			t.Fatal("game must still be paused after the aborted countdown elapsed")
		}
	})
}

func TestCompetitiveLeaveCancelsCountdownAndKeepsAutoPause(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)
		clock.Spawned(p2)

		clock.Pause(p1)
		clock.Resume(p1)
		clock.Leave(p2)

		clock.casualClock.mu.Lock()
		if clock.countdown != nil {
			clock.casualClock.mu.Unlock()
			t.Fatal("a player leaving must cancel the resume countdown")
		}
		clock.casualClock.mu.Unlock()

		time.Sleep(2 * resumeCountdown)
		if !clock.Paused() {
			t.Fatal("game must remain paused after a player left")
		}

		// p1's manual holder is still there, and the player-left holder too:
		// bringing p2 back must not be enough while p1's manual pause holds
		p2.State = playerstate.Dead
		clock.Join(p2)
		if !clock.Paused() {
			t.Fatal("manual pause holder must keep the game paused after auto-pause is released")
		}

		clock.Resume(nil) // authoritative system resume
		if clock.Paused() {
			t.Fatal("system resume must clear manual holders")
		}
	})
}

func TestCompetitiveRepeatedSpectatorToggles(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)
		clock.Spawned(p2)

		for i := 0; i < 3; i++ {
			// player goes to spectator: the clock is notified (while the player
			// is still in a playing state), then the state flips
			clock.Leave(p1)
			p1.State = playerstate.Spectator
			if !clock.Paused() {
				t.Fatalf("iteration %d: game must pause when a player spectates", i)
			}
			frozen := clock.TimeLeft()
			time.Sleep(30 * time.Millisecond)
			if got := clock.TimeLeft(); got != frozen {
				t.Fatalf("iteration %d: time moved while paused: %v -> %v", i, frozen, got)
			}

			// player comes back: state flips, then the clock is notified
			p1.State = playerstate.Dead
			clock.Join(p1)
			if clock.Paused() {
				t.Fatalf("iteration %d: game must resume when the player is back", i)
			}
		}
	})
}

func TestCompetitiveDisconnectLeavesNoStrandedHolder(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)
		clock.Spawned(p2)

		// p1 manually pauses, then drops while playing
		clock.Pause(p1)
		clock.Disconnect(p1)

		clock.casualClock.mu.Lock()
		for h := range clock.holders {
			if h.cn == int32(p1.CN) {
				clock.casualClock.mu.Unlock()
				t.Fatalf("holder referencing disconnected player %d remains: %+v", p1.CN, h)
			}
		}
		autoPause := clock.holderCount(playerLeftPause)
		clock.casualClock.mu.Unlock()
		if autoPause != 1 {
			t.Fatalf("expected player-left auto-pause after disconnect, got %d", autoPause)
		}
		if !clock.Paused() {
			t.Fatal("game must stay paused waiting for a player to come back")
		}

		// player rejoins: auto-pause is released by the eligible event
		p1.State = playerstate.Dead
		clock.Join(p1)
		if clock.Paused() {
			t.Fatal("game must resume once the player slot is filled again")
		}
	})
}

func TestCompetitiveSpectatorDisconnectReleasesManualPause(t *testing.T) {
	p1 := testPlayer(1, "alice")
	spec := spectator(2, "bob")
	srv := newMockServer(time.Minute, p1, spec)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)

		// a spectator holding a manual pause disconnects
		clock.Pause(spec)
		if !clock.Paused() {
			t.Fatal("expected spectator pause to hold")
		}
		clock.Disconnect(spec)
		if clock.Paused() {
			t.Fatal("spectator disconnect must release their pause, no stranded holder")
		}
	})
}

func TestCompetitiveLeaveDuringSpawnWait(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	clock.Start()
	if !clock.Paused() {
		t.Fatal("expected spawn-wait pause")
	}

	// p1 gives up waiting and switches to spectator (Leave runs first, then
	// the state change, matching the production call order)
	clock.Leave(p1)
	p1.State = playerstate.Spectator
	if !clock.Paused() {
		t.Fatal("game must keep waiting for the remaining player")
	}

	// last pending player spawns: the spawn wait ends, but the player-left
	// pause still holds until the missing player comes back
	clock.Spawned(p2)
	if !clock.Paused() {
		t.Fatal("player-left auto-pause must keep the game paused after the spawn wait ends")
	}

	// p1 rejoins: the eligible event releases the auto-pause
	p1.State = playerstate.Dead
	clock.Join(p1)
	if clock.Paused() {
		t.Fatal("game must resume once the missing player is back")
	}
}

func TestCompetitiveStopAndFreshStartAreClean(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	clock.Start()
	clock.Spawned(p1) // p2 still loading
	clock.Pause(p1)
	clock.Stop()

	clock.casualClock.mu.Lock()
	holders := len(clock.holders)
	pending := len(clock.pendingSpawns)
	countdown := clock.countdown
	clock.casualClock.mu.Unlock()
	if holders != 0 || pending != 0 || countdown != nil {
		t.Fatalf("Stop left state behind: holders=%d pending=%d countdown=%v", holders, pending, countdown)
	}
	if clock.Paused() {
		t.Fatal("clock must not be paused after Stop")
	}

	// map change / empty-server reset builds a brand-new clock, as StartGame does
	fresh := NewCompetitiveClock(newMockServer(time.Minute), nil)
	fresh.Start()
	if fresh.Paused() {
		t.Fatal("new game must start from a clean, running state")
	}
}

func TestCompetitiveManualPauseSurvivesAutoPauseLifecycle(t *testing.T) {
	p1 := testPlayer(1, "alice")
	p2 := testPlayer(2, "bob")
	srv := newMockServer(time.Minute, p1, p2)
	clock := NewCompetitiveClock(srv, nil)

	withShortCountdown(t, func() {
		clock.Start()
		clock.Spawned(p1)
		clock.Spawned(p2)

		// admin manually pauses, then a player leaves and comes back
		clock.Pause(p1)
		clock.Leave(p2)
		p2.State = playerstate.Dead
		clock.Join(p2)

		if !clock.Paused() {
			t.Fatal("manual pause must still hold after the auto-pause was released")
		}
		clock.casualClock.mu.Lock()
		manual := clock.holderCount(manualPause)
		auto := clock.holderCount(playerLeftPause)
		clock.casualClock.mu.Unlock()
		if manual != 1 || auto != 0 {
			t.Fatalf("expected 1 manual holder and 0 auto holders, got manual=%d auto=%d", manual, auto)
		}

		// only releasing the manual holder (via countdown) resumes the game
		clock.Resume(p1)
		waitFor(t, "manual resume countdown", 3*resumeCountdown, func() bool {
			return !clock.Paused()
		})
	})
}
