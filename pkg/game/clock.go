package game

import (
	"log"
	"sync"
	"time"

	"github.com/sauerbraten/timer"
	"github.com/sauerbraten/waiter/pkg/protocol/nmc"
	"github.com/sauerbraten/waiter/pkg/protocol/playerstate"
)

// implemented by modes with modes with either flags or pickups
type hasTimers interface {
	// Start() // flags/pickups "auto-start" once they are received from a client
	Pause()
	Resume()
	Stop()
	CleanUp()
}

var (
	_ hasTimers = &flagMode{}
	_ hasTimers = &handlesPickups{}
)

// pauseReason classifies why the game clock is paused.
type pauseReason int

const (
	// a player (or the server itself, with cn -1) asked to pause the game
	manualPause pauseReason = iota
	// competitive mode: a player left the game, the game stays paused
	// until an eligible player is back
	playerLeftPause
	// competitive mode: the game waits for all players to load the map
	waitingForSpawnsPause
)

// pauseHolder is one reason the game clock is paused. The clock only runs
// once every holder has been released. Holders are identified by (reason, cn),
// so the same player pausing repeatedly can never stack or flip state.
type pauseHolder struct {
	reason pauseReason
	cn     int32
}

// resumeCountdown is the delay between a (manual) resume request in
// competitive mode and the clock actually running again.
var resumeCountdown = 3 * 650 * time.Millisecond

type casualClock struct {
	s Server
	t *timer.Timer
	m hasTimers

	mu      sync.Mutex
	holders map[pauseHolder]struct{}
}

func NewCasualClock(s Server, m Mode) *casualClock {
	timedMode, _ := m.(hasTimers) // nil if mode has no timers
	return &casualClock{
		s:       s,
		t:       timer.AfterFunc(s.GameDuration(), s.Intermission),
		m:       timedMode,
		holders: map[pauseHolder]struct{}{},
	}
}

func (c *casualClock) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t.Start()
	log.Println("started game timer, time left:", c.t.TimeLeft())
	c.s.Broadcast(nmc.TimeLeft, int32(c.t.TimeLeft()/time.Second))
}

// pauseTimer stops the underlying timer (and mode timers) without touching
// the holder set. Callers must hold c.mu.
func (c *casualClock) pauseTimer(cn int32) {
	if c.t.Paused() {
		return
	}
	c.s.Broadcast(nmc.PauseGame, 1, cn)
	c.t.Pause()
	if c.m != nil {
		c.m.Pause()
	}
}

// resumeTimer restarts the underlying timer (and mode timers) without touching
// the holder set. Callers must hold c.mu.
func (c *casualClock) resumeTimer(cn int32) {
	if !c.t.Paused() {
		return
	}
	c.s.Broadcast(nmc.PauseGame, 0, cn)
	c.t.Start()
	if c.m != nil {
		c.m.Resume()
	}
}

// holderCN returns the CN identifying a pause/release request, -1 for the
// server itself (no concrete player).
func holderCN(p *Player) int32 {
	if p == nil {
		return -1
	}
	return int32(p.CN)
}

// Pause records a manual pause holder for p (or the server when p is nil) and
// stops the clock while any holder exists. Repeated pauses by the same player
// do not stack.
func (c *casualClock) Pause(p *Player) {
	cn := holderCN(p)

	c.mu.Lock()
	defer c.mu.Unlock()

	h := pauseHolder{reason: manualPause, cn: cn}
	if _, ok := c.holders[h]; ok {
		return
	}
	c.holders[h] = struct{}{}
	c.pauseTimer(cn)
}

func (c *casualClock) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.holders) > 0
}

// holderCount returns how many holders of the given reason exist.
// Callers must hold c.mu.
func (c *casualClock) holderCount(reason pauseReason) int {
	n := 0
	for h := range c.holders {
		if h.reason == reason {
			n++
		}
	}
	return n
}

// Resume releases the manual pause holder belonging to p. In casual games any
// eligible resume request clears every manual holder; in competitive games the
// embedding clock overrides this so auto-pause holders can never be released
// this way. The clock only runs again once no holder of any reason remains.
func (c *casualClock) Resume(p *Player) {
	cn := holderCN(p)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.releaseManual(cn)
	if len(c.holders) == 0 {
		c.resumeTimer(cn)
	}
}

// releaseManual removes manual pause holders. For a concrete player only that
// player's holder is removed; a server-side request (cn -1) clears all manual
// holders. Callers must hold c.mu.
func (c *casualClock) releaseManual(cn int32) {
	for h := range c.holders {
		if h.reason != manualPause {
			continue
		}
		if cn == -1 || h.cn == cn {
			delete(c.holders, h)
		}
	}
}

// releaseHoldersOf removes every holder (of any reason) pointing at p.
// Callers must hold c.mu.
func (c *casualClock) releaseHoldersOf(cn int32) {
	for h := range c.holders {
		if h.cn == cn {
			delete(c.holders, h)
		}
	}
}

// addHolder records h and stops the timer on the first holder. Callers must
// hold c.mu.
func (c *casualClock) addHolder(h pauseHolder, broadcastCN int32) {
	if _, ok := c.holders[h]; ok {
		return
	}
	c.holders[h] = struct{}{}
	c.pauseTimer(broadcastCN)
}

// purgePlayer removes all pause holders referencing p and restarts the timer
// if no holder remains. It never leaves a holder pointing at a player who is
// gone, so a disconnecting player can never keep the game paused forever.
func (c *casualClock) purgePlayer(cn int32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.releaseHoldersOf(cn)
	if len(c.holders) == 0 {
		c.resumeTimer(-1)
	}
}

func (*casualClock) Join(*Player)      {}
func (*casualClock) Spawned(p *Player) {}
func (*casualClock) Leave(*Player)     {}

// Disconnect is called when a client's connection is gone, regardless of
// whether they were playing or spectating.
func (c *casualClock) Disconnect(p *Player) {
	c.purgePlayer(holderCN(p))
}

func (c *casualClock) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.holders = map[pauseHolder]struct{}{}
	c.s.Broadcast(nmc.TimeLeft, 0)
	log.Println("stopping game timer, time left:", c.t.TimeLeft())
	c.t.Stop()
	if c.m != nil {
		c.m.Stop()
	}
}

func (c *casualClock) Ended() bool {
	return c.t.TimeLeft() <= 0
}

func (c *casualClock) TimeLeft() time.Duration {
	return c.t.TimeLeft()
}

func (c *casualClock) SetTimeLeft(d time.Duration) {
	log.Println("setting game timer to", d)
	if !c.t.SetTimeLeft(d) {
		log.Println("game timer had already expired")
	}
	c.s.Broadcast(nmc.TimeLeft, int32(d/time.Second))
}

type competitiveClock struct {
	*casualClock

	// players that still have to send their first spawn after map load
	pendingSpawns map[uint32]struct{}

	// a manual resume countdown currently in flight
	countdown      []*time.Timer
	countdownEpoch int
}

func NewCompetitiveClock(s Server, m Mode) *competitiveClock {
	return &competitiveClock{
		casualClock:   NewCasualClock(s, m),
		pendingSpawns: map[uint32]struct{}{},
	}
}

func (c *competitiveClock) Start() {
	c.casualClock.mu.Lock()
	c.cancelResumeCountdownLocked()
	c.holders = map[pauseHolder]struct{}{}
	c.pendingSpawns = map[uint32]struct{}{}
	c.casualClock.mu.Unlock()

	c.casualClock.Start()

	c.s.ForEachPlayer(func(p *Player) {
		if p.State != playerstate.Spectator {
			c.pendingSpawns[p.CN] = struct{}{}
		}
	})

	if len(c.pendingSpawns) == 0 {
		return
	}

	c.casualClock.mu.Lock()
	c.addHolder(pauseHolder{reason: waitingForSpawnsPause, cn: -1}, -1)
	c.casualClock.mu.Unlock()

	c.s.Broadcast(nmc.ServerMessage, "waiting for all players to load the map")
}

func (c *competitiveClock) Join(p *Player) {
	if p.State == playerstate.Spectator {
		return
	}

	c.casualClock.mu.Lock()
	defer c.casualClock.mu.Unlock()

	if c.holderCount(waitingForSpawnsPause) > 0 {
		// the game is still waiting for map loads: this player has to load too
		c.pendingSpawns[p.CN] = struct{}{}
		return
	}

	// a player came back: release the auto-pause caused by a missing player
	h := pauseHolder{reason: playerLeftPause, cn: -1}
	if _, ok := c.holders[h]; !ok {
		return
	}
	delete(c.holders, h)
	c.s.Broadcast(nmc.ServerMessage, "a player is back, continuing the game")
	if len(c.holders) == 0 {
		c.resumeTimer(int32(p.CN))
	}
}

func (c *competitiveClock) Spawned(p *Player) {
	c.casualClock.mu.Lock()
	defer c.casualClock.mu.Unlock()

	if _, ok := c.pendingSpawns[p.CN]; !ok {
		return
	}
	delete(c.pendingSpawns, p.CN)

	if len(c.pendingSpawns) > 0 {
		return
	}

	// everyone loaded the map; the waiting holder is the only event allowed
	// to release this auto-pause
	h := pauseHolder{reason: waitingForSpawnsPause, cn: -1}
	if _, ok := c.holders[h]; !ok {
		return
	}
	delete(c.holders, h)
	c.s.Broadcast(nmc.ServerMessage, "all players spawned, starting game")
	if len(c.holders) == 0 {
		c.resumeTimer(-1)
	}
}

// Pause records a manual pause holder. If a resume countdown is in flight the
// countdown is aborted; the existing holders (including auto-pause holders)
// stay untouched. Repeated pauses by the same player are idempotent.
func (c *competitiveClock) Pause(p *Player) {
	cn := holderCN(p)

	c.casualClock.mu.Lock()
	defer c.casualClock.mu.Unlock()

	c.cancelResumeCountdownLocked()
	c.addHolder(pauseHolder{reason: manualPause, cn: cn}, cn)
}

// Resume handles resume requests. A player request can only release the
// requesting player's own manual pause holder and starts the shared resume
// countdown; auto-pause holders (player left, waiting for spawns) are never
// released by a manual request, so ordinary players cannot bypass them.
// A server-side request (p == nil) is an authoritative system resume that
// clears all manual holders immediately.
func (c *competitiveClock) Resume(p *Player) {
	cn := holderCN(p)

	c.casualClock.mu.Lock()
	defer c.casualClock.mu.Unlock()

	if p == nil {
		c.cancelResumeCountdownLocked()
		c.releaseManual(-1)
		if len(c.holders) == 0 {
			c.resumeTimer(cn)
		}
		return
	}

	if c.countdown != nil {
		// a resume is already in flight, repeated requests must not stack
		return
	}

	if _, ok := c.holders[pauseHolder{reason: manualPause, cn: cn}]; !ok {
		// nothing this request is allowed to release; auto-pauses stay in effect
		return
	}

	if p != nil {
		c.s.Broadcast(nmc.ServerMessage, c.s.UniqueName(p)+" wants to resume the game")
	}
	c.s.Broadcast(nmc.ServerMessage, "resuming game in 3")

	epoch := c.countdownEpoch
	c.countdown = []*time.Timer{
		time.AfterFunc(resumeCountdown/3, func() {
			c.casualClock.mu.Lock()
			current := c.countdownEpoch
			c.casualClock.mu.Unlock()
			if current == epoch {
				c.s.Broadcast(nmc.ServerMessage, "... 2")
			}
		}),
		time.AfterFunc(2*resumeCountdown/3, func() {
			c.casualClock.mu.Lock()
			current := c.countdownEpoch
			c.casualClock.mu.Unlock()
			if current == epoch {
				c.s.Broadcast(nmc.ServerMessage, "... 1")
			}
		}),
		time.AfterFunc(resumeCountdown, func() {
			c.casualClock.mu.Lock()
			defer c.casualClock.mu.Unlock()
			if c.countdownEpoch != epoch {
				return
			}
			c.countdown = nil
			c.releaseManual(cn)
			if len(c.holders) == 0 {
				c.resumeTimer(cn)
			}
		}),
	}
}

// cancelResumeCountdownLocked aborts an in-flight resume countdown.
// Callers must hold c.casualClock.mu.
func (c *competitiveClock) cancelResumeCountdownLocked() {
	if c.countdown == nil {
		return
	}
	for _, action := range c.countdown {
		if action != nil {
			action.Stop()
		}
	}
	c.countdown = nil
	c.countdownEpoch++
	c.s.Broadcast(nmc.ServerMessage, "resuming aborted")
}

// Leave is called when a playing player leaves the game (disconnect or switch
// to spectator); the player's state is still a playing state at this point.
// In competitive mode the game auto-pauses until an eligible player is back.
// Any holder pointing at the leaving player is removed first, so leaving can
// never strand the clock in pause.
func (c *competitiveClock) Leave(p *Player) {
	cn := holderCN(p)

	c.casualClock.mu.Lock()
	defer c.casualClock.mu.Unlock()

	c.cancelResumeCountdownLocked()
	c.releaseHoldersOf(cn)
	delete(c.pendingSpawns, uint32(cn))

	if c.Ended() {
		if len(c.pendingSpawns) == 0 {
			delete(c.holders, pauseHolder{reason: waitingForSpawnsPause, cn: -1})
		}
		if len(c.holders) == 0 {
			c.resumeTimer(cn)
		}
		return
	}

	c.s.Broadcast(nmc.ServerMessage, "a player left the game")
	c.addHolder(pauseHolder{reason: playerLeftPause, cn: -1}, cn)
}

// Disconnect is called when a client's connection is gone, regardless of
// whether they were playing or spectating. No holder referencing the
// disconnected player may survive, otherwise the game would stay paused
// forever.
func (c *competitiveClock) Disconnect(p *Player) {
	cn := holderCN(p)

	c.casualClock.mu.Lock()
	defer c.casualClock.mu.Unlock()

	c.cancelResumeCountdownLocked()
	c.releaseHoldersOf(cn)
	delete(c.pendingSpawns, uint32(cn))

	wasSpectator := p.State == playerstate.Spectator

	if len(c.pendingSpawns) == 0 {
		delete(c.holders, pauseHolder{reason: waitingForSpawnsPause, cn: -1})
	}

	if !wasSpectator && !c.Ended() {
		// the game keeps waiting for an eligible player to come back,
		// but the holder no longer points at the disconnected player
		c.s.Broadcast(nmc.ServerMessage, "a player left the game")
		c.addHolder(pauseHolder{reason: playerLeftPause, cn: -1}, -1)
		return
	}

	if len(c.holders) == 0 {
		c.resumeTimer(-1)
	}
}

func (c *competitiveClock) Stop() {
	c.casualClock.mu.Lock()
	c.cancelResumeCountdownLocked()
	c.pendingSpawns = map[uint32]struct{}{}
	c.casualClock.mu.Unlock()

	c.casualClock.Stop()
}

func (c *competitiveClock) ToCasual() *casualClock { return c.casualClock }
