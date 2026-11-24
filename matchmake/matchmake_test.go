package matchmake

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tengen-io/server/models"
)

// publishedEvent is a notification captured by the fake pool.
type publishedEvent struct {
	subject string
	event   string
	gameId  string
}

// failurePoint selects which step of a simulated match transaction should
// fail. All failures must behave the same way: full rollback, both requests
// retained, no game and no notifications left behind.
type failurePoint int

const (
	failNone failurePoint = iota
	failCreateGame
	failDeleteFirst
	failDeleteSecond
	failPublishFirst
	failPublishSecond
)

// fakeGame is a game created inside the fake store.
type fakeGame struct {
	id      string
	state   models.GameState
	userIds []string
}

// fakePool is an in-memory pool that models the transactional guarantees of
// the real database-backed pool: match() only applies its changes (game
// creation, request deletion, both notifications) when every step succeeds;
// any injected failure rolls everything back atomically.
type fakePool struct {
	mu sync.Mutex

	queued []models.MatchmakingRequest
	games  []fakeGame
	events []publishedEvent

	// failOn controls the next match() call; reset to failNone afterwards.
	failOn failurePoint
	// failRequests makes the requests() call return an error, used to prove
	// the run loop survives a failed scan.
	failRequests bool
	// nextId hands out fresh request ids.
	nextId int64
	// nextGameId hands out fresh game ids.
	nextGameId int64
	// matchCalls records every attempted pairing (including rolled-back ones).
	matchCalls []struct{ i, j string }
}

func newFakePool() *fakePool {
	return &fakePool{
		queued:     make([]models.MatchmakingRequest, 0),
		games:      make([]fakeGame, 0),
		events:     make([]publishedEvent, 0),
		failOn:     failNone,
		nextId:     1,
		nextGameId: 1,
	}
}

func (p *fakePool) requests() ([]models.MatchmakingRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.failRequests {
		return nil, errors.New("injected requests() failure")
	}

	// Return a copy so callers cannot mutate the store directly.
	out := make([]models.MatchmakingRequest, len(p.queued))
	copy(out, p.queued)
	return out, nil
}

// enqueue is the in-memory counterpart of
// Repository.CreateMatchmakingRequest: a repeat submission for a user that
// already has a queued request returns the existing request unchanged and
// keeps its original rank_delta.
func (p *fakePool) enqueue(user models.User, delta int) (*models.MatchmakingRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for k := range p.queued {
		if p.queued[k].User.Id == user.Id {
			existing := p.queued[k]
			return &existing, nil
		}
	}

	req := models.MatchmakingRequest{
		NodeFields: models.NodeFields{Id: strconv.FormatInt(p.nextId, 10)},
		Queue:      "FIXME",
		User:       user,
		Rank:       10,
		Delta:      delta,
	}
	p.nextId++
	p.queued = append(p.queued, req)
	return &req, nil
}

func (p *fakePool) match(i, j models.MatchmakingRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.matchCalls = append(p.matchCalls, struct{ i, j string }{i.Id, j.Id})
	fail := p.failOn
	p.failOn = failNone

	// Stage every change locally first; nothing is committed until all steps
	// succeed, mirroring a single database transaction.
	gameId := strconv.FormatInt(p.nextGameId, 10)
	p.nextGameId++

	stagedGame := fakeGame{
		id:      gameId,
		state:   models.GameStateNegotiation,
		userIds: []string{i.User.Id, j.User.Id},
	}
	if fail == failCreateGame {
		return errors.New("injected CreateGame failure")
	}

	remaining := make([]models.MatchmakingRequest, 0, len(p.queued))
	deletedFirst := false
	deletedSecond := false
	for _, req := range p.queued {
		switch req.Id {
		case i.Id:
			deletedFirst = true
			continue
		case j.Id:
			deletedSecond = true
			continue
		}
		remaining = append(remaining, req)
	}
	if fail == failDeleteFirst {
		return errors.New("injected first DeleteMatchmakingRequest failure")
	}
	if fail == failDeleteSecond {
		return errors.New("injected second DeleteMatchmakingRequest failure")
	}

	stagedEvents := []publishedEvent{
		{subject: i.User.Id, event: "matched", gameId: gameId},
		{subject: j.User.Id, event: "matched", gameId: gameId},
	}
	if fail == failPublishFirst {
		return errors.New("injected first Publish failure")
	}
	if fail == failPublishSecond {
		return errors.New("injected second Publish failure")
	}

	// All steps succeeded: commit atomically.
	if !deletedFirst || !deletedSecond {
		return fmt.Errorf("cannot match: request %s or %s is no longer queued", i.Id, j.Id)
	}
	p.queued = remaining
	p.games = append(p.games, stagedGame)
	p.events = append(p.events, stagedEvents...)
	return nil
}

// snapshot helpers -----------------------------------------------------------

func (p *fakePool) queuedIds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.queued))
	for _, req := range p.queued {
		ids = append(ids, req.Id)
	}
	return ids
}

func (p *fakePool) requestForUser(userId string) *models.MatchmakingRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.queued {
		if p.queued[k].User.Id == userId {
			req := p.queued[k]
			return &req
		}
	}
	return nil
}

func (p *fakePool) gameCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.games)
}

func (p *fakePool) eventSubjects() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	subjects := make([]string, 0, len(p.events))
	for _, ev := range p.events {
		subjects = append(subjects, ev.subject)
	}
	return subjects
}

// setRequests replaces the queued requests. It takes the lock so it is safe
// to call while the run loop is polling the pool.
func (p *fakePool) setRequests(reqs []models.MatchmakingRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queued = reqs
}

// setFailRequests toggles the injected requests() failure under the lock.
func (p *fakePool) setFailRequests(fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failRequests = fail
}

// setFailOn arms a one-shot match() failure under the lock.
func (p *fakePool) setFailOn(f failurePoint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failOn = f
}

func (p *fakePool) matchCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.matchCalls)
}

// test request builder: id, user, rank, delta.
func r(id int64, user int, rank, delta int) models.MatchmakingRequest {
	return models.MatchmakingRequest{
		NodeFields: models.NodeFields{Id: strconv.FormatInt(id, 10)},
		Queue:      "",
		User: models.User{
			NodeFields: models.NodeFields{
				Id: strconv.Itoa(user),
			},
		},
		Rank:  rank,
		Delta: delta,
	}
}

func user(id int) models.User {
	return models.User{NodeFields: models.NodeFields{Id: strconv.Itoa(id)}}
}

// --- Idempotent enqueue -----------------------------------------------------

func Test_enqueue_idempotent(t *testing.T) {
	pool := newFakePool()

	first, err := pool.enqueue(user(1), 5)
	assert.NoError(t, err)
	assert.Len(t, pool.queuedIds(), 1)
	assert.Equal(t, 5, first.Delta)

	// Repeated clicks must not create a second row, even with a different
	// delta: the original request and its rank_delta are returned.
	second, err := pool.enqueue(user(1), 99)
	assert.NoError(t, err)
	assert.Len(t, pool.queuedIds(), 1)
	assert.Equal(t, first.Id, second.Id)
	assert.Equal(t, 5, second.Delta, "original rank_delta must be preserved")

	third, err := pool.enqueue(user(1), 7)
	assert.NoError(t, err)
	assert.Equal(t, first.Id, third.Id)
	assert.Equal(t, 5, third.Delta)
	assert.Len(t, pool.queuedIds(), 1)

	// A different user still gets their own request.
	other, err := pool.enqueue(user(2), 3)
	assert.NoError(t, err)
	assert.Len(t, pool.queuedIds(), 2)
	assert.NotEqual(t, first.Id, other.Id)
	assert.Equal(t, 3, other.Delta)
}

// --- tick: no duplicate / self pairing within one scan ----------------------

func Test_tick(t *testing.T) {
	testCases := []struct {
		name     string
		requests []models.MatchmakingRequest
		expected []struct{ i, j string }
	}{
		{
			"within delta",
			[]models.MatchmakingRequest{r(1, 1, 5, 3), r(2, 2, 3, 3)},
			[]struct{ i, j string }{{"1", "2"}},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			pool := newFakePool()
			pool.queued = testCase.requests

			matchmaker := newMatchmaker(pool, time.Duration(1)*time.Second)
			matches, err := matchmaker.tick()
			assert.NoError(t, err)

			found := 0
		expected:
			for _, expected := range testCase.expected {
				for _, match := range matches {
					if (match.i.Id == expected.i || match.i.Id == expected.j) &&
						(match.j.Id == expected.i || match.j.Id == expected.j) {
						found++
						continue expected
					}
				}
			}

			assert.Len(t, testCase.expected, found)
		})
	}
}

func Test_tick_eachPlayerMatchedAtMostOnce(t *testing.T) {
	// Three mutually-compatible requests: the scan must produce exactly one
	// pair and leave one player unmatched rather than putting anyone in two
	// pairs.
	pool := newFakePool()
	pool.queued = []models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 2, 11, 100),
		r(3, 3, 12, 100),
	}

	matchmaker := newMatchmaker(pool, time.Second)
	matches, err := matchmaker.tick()
	assert.NoError(t, err)
	assert.Len(t, matches, 1)

	seenUsers := map[string]bool{}
	for _, m := range matches {
		assert.NotEqual(t, m.i.User.Id, m.j.User.Id)
		assert.False(t, seenUsers[m.i.User.Id], "user appears in more than one pair")
		assert.False(t, seenUsers[m.j.User.Id], "user appears in more than one pair")
		seenUsers[m.i.User.Id] = true
		seenUsers[m.j.User.Id] = true
	}
}

func Test_tick_noDuplicatePairingAcrossManyPlayers(t *testing.T) {
	// Four compatible requests -> two distinct pairs, no player repeated and
	// no pair produced twice.
	pool := newFakePool()
	pool.queued = []models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 2, 11, 100),
		r(3, 3, 12, 100),
		r(4, 4, 13, 100),
	}

	matchmaker := newMatchmaker(pool, time.Second)
	matches, err := matchmaker.tick()
	assert.NoError(t, err)
	assert.Len(t, matches, 2)

	pairSeen := map[string]bool{}
	userSeen := map[string]bool{}
	for _, m := range matches {
		a, b := m.i.User.Id, m.j.User.Id
		if a > b {
			a, b = b, a
		}
		key := a + "|" + b
		assert.False(t, pairSeen[key], "same pair matched twice in one scan")
		pairSeen[key] = true

		assert.False(t, userSeen[m.i.User.Id], "user matched twice")
		assert.False(t, userSeen[m.j.User.Id], "user matched twice")
		userSeen[m.i.User.Id] = true
		userSeen[m.j.User.Id] = true
	}
}

func Test_tick_neverSelfPairs(t *testing.T) {
	// Simulate the bug condition the idempotent enqueue prevents: two queued
	// rows for the same user. The matchmaker must never pair the user with
	// themselves; only the other distinct user is eligible.
	pool := newFakePool()
	pool.queued = []models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 1, 10, 100), // duplicate row for user 1
		r(3, 2, 11, 100),
	}

	matchmaker := newMatchmaker(pool, time.Second)
	matches, err := matchmaker.tick()
	assert.NoError(t, err)
	assert.Len(t, matches, 1)
	assert.NotEqual(t, matches[0].i.User.Id, matches[0].j.User.Id)

	ids := map[string]bool{matches[0].i.User.Id: true, matches[0].j.User.Id: true}
	assert.True(t, ids["1"] && ids["2"])
}

func Test_tick_noMatchOutsideDelta(t *testing.T) {
	pool := newFakePool()
	pool.queued = []models.MatchmakingRequest{
		r(1, 1, 10, 1),
		r(2, 2, 50, 1),
	}

	matchmaker := newMatchmaker(pool, time.Second)
	matches, err := matchmaker.tick()
	assert.NoError(t, err)
	assert.Empty(t, matches)
}

// --- Atomic match transaction: rollback then retry --------------------------

func Test_match_successCommitsEverything(t *testing.T) {
	pool := newFakePool()
	pool.queued = []models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 2, 11, 100),
	}
	reqs, _ := pool.requests()

	err := pool.match(reqs[0], reqs[1])
	assert.NoError(t, err)

	assert.Empty(t, pool.queuedIds(), "both requests removed on success")
	assert.Equal(t, 1, pool.gameCount())
	assert.ElementsMatch(t, []string{"1", "2"}, pool.eventSubjects())
}

func Test_match_failuresRollBackAndRetainBothRequests(t *testing.T) {
	cases := []struct {
		name   string
		failOn failurePoint
	}{
		{"create game failure", failCreateGame},
		{"first request deletion failure", failDeleteFirst},
		{"second request deletion failure", failDeleteSecond},
		{"first notification failure", failPublishFirst},
		{"second notification failure", failPublishSecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newFakePool()
			pool.queued = []models.MatchmakingRequest{
				r(1, 1, 10, 100),
				r(2, 2, 11, 100),
			}
			reqs, _ := pool.requests()
			pool.failOn = tc.failOn

			err := pool.match(reqs[0], reqs[1])
			assert.Error(t, err)

			// Full rollback: both requests retained, no half-finished game,
			// and not even one notification leaked.
			assert.ElementsMatch(t, []string{"1", "2"}, pool.queuedIds())
			assert.Equal(t, 0, pool.gameCount(), "no game may survive the rollback")
			assert.Empty(t, pool.eventSubjects(), "no notification may be delivered on failure")

			// The retained requests keep their original identity and delta so
			// the next scan can pair them again unchanged.
			req1 := pool.requestForUser("1")
			req2 := pool.requestForUser("2")
			assert.NotNil(t, req1)
			assert.NotNil(t, req2)
			assert.Equal(t, "1", req1.Id)
			assert.Equal(t, 100, req1.Delta)
			assert.Equal(t, "2", req2.Id)
			assert.Equal(t, 100, req2.Delta)
		})
	}
}

func Test_match_rolledBackRequestsRetryOnNextScan(t *testing.T) {
	pool := newFakePool()
	pool.queued = []models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 2, 11, 100),
	}
	matchmaker := newMatchmaker(pool, time.Second)

	// First scan pairs the players but the transaction fails (simulate a
	// notification failure): everything rolls back.
	matches, err := matchmaker.tick()
	assert.NoError(t, err)
	assert.Len(t, matches, 1)
	pool.failOn = failPublishFirst
	assert.Error(t, pool.match(matches[0].i, matches[0].j))
	assert.Len(t, pool.queuedIds(), 2)
	assert.Equal(t, 0, pool.gameCount())

	// Next scan finds the same requests again and now commits successfully.
	matches, err = matchmaker.tick()
	assert.NoError(t, err)
	assert.Len(t, matches, 1)
	assert.NoError(t, pool.match(matches[0].i, matches[0].j))

	assert.Empty(t, pool.queuedIds())
	assert.Equal(t, 1, pool.gameCount())
	assert.ElementsMatch(t, []string{"1", "2"}, pool.eventSubjects())
}

func Test_match_deliversExactlyTwoNotifications(t *testing.T) {
	pool := newFakePool()
	pool.queued = []models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 2, 11, 100),
	}
	reqs, _ := pool.requests()

	assert.NoError(t, pool.match(reqs[0], reqs[1]))

	events := append([]publishedEvent(nil), pool.events...)
	assert.Len(t, events, 2)
	subjects := map[string]int{}
	gameIds := map[string]bool{}
	for _, ev := range events {
		assert.Equal(t, "matched", ev.event)
		subjects[ev.subject]++
		gameIds[ev.gameId] = true
	}
	assert.Equal(t, 1, subjects["1"], "player 1 notified exactly once")
	assert.Equal(t, 1, subjects["2"], "player 2 notified exactly once")
	assert.Len(t, gameIds, 1, "both notifications reference the same game")
}

// --- Run loop keeps going after failures ------------------------------------

func Test_run_survivesFailedScansAndFailedMatches(t *testing.T) {
	pool := newFakePool()
	// First scans fail at the storage layer.
	pool.setFailRequests(true)

	matchmaker := newMatchmaker(pool, 5*time.Millisecond)
	go matchmaker.run()

	// Give the loop time to survive several failing scans.
	time.Sleep(40 * time.Millisecond)
	pool.setFailRequests(false)

	// Now enqueue a pairable set; the still-running loop must pick them up.
	pool.setRequests([]models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 2, 11, 100),
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && pool.gameCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	assert.Equal(t, 1, pool.gameCount(), "loop recovered from failed scans and completed a match")
	assert.Empty(t, pool.queuedIds())
	assert.ElementsMatch(t, []string{"1", "2"}, pool.eventSubjects())
}

func Test_run_continuesAfterMatchTransactionFailure(t *testing.T) {
	pool := newFakePool()
	pool.setRequests([]models.MatchmakingRequest{
		r(1, 1, 10, 100),
		r(2, 2, 11, 100),
	})
	// First attempted match fails at notification time.
	pool.setFailOn(failPublishSecond)

	matchmaker := newMatchmaker(pool, 5*time.Millisecond)
	go matchmaker.run()

	// After the failed attempt the requests remain; the loop retries on
	// subsequent ticks and eventually commits.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && pool.gameCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	assert.Equal(t, 1, pool.gameCount())
	assert.Empty(t, pool.queuedIds())
	if pool.matchCallCount() < 2 {
		t.Fatalf("expected the failed attempt to be retried, got %d match attempts", pool.matchCallCount())
	}
}
