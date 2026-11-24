package repository

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tengen-io/server/models"
	"github.com/tengen-io/server/test"
)

// createTestUser inserts a user and returns it. Test fixtures already
// provide user 1; every call here creates a fresh user so tests stay
// independent of execution order.
func createTestUser(t *testing.T, r *Repository, name string) models.User {
	t.Helper()

	identity, err := r.CreateIdentity(
		"mm-"+name+"@tengen.io",
		[]byte("hunter2"),
		name,
	)
	assert.NoError(t, err)
	return identity.User
}

func TestRepository_CreateMatchmakingRequest_isIdempotent(t *testing.T) {
	r := NewRepository(test.DB(), test.PubSub())
	user := createTestUser(t, r, "idempotent")

	first, err := r.CreateMatchmakingRequest(user, 4)
	assert.NoError(t, err)
	assert.NotNil(t, first)
	assert.Equal(t, 4, first.Delta)

	// A repeat submission with a different delta must return the same row
	// and leave the original rank_delta untouched.
	second, err := r.CreateMatchmakingRequest(user, 99)
	assert.NoError(t, err)
	assert.NotNil(t, second)
	assert.Equal(t, first.Id, second.Id, "repeat enqueue must return the existing request")
	assert.Equal(t, 4, second.Delta, "original rank_delta must be preserved")

	third, err := r.CreateMatchmakingRequest(user, 7)
	assert.NoError(t, err)
	assert.Equal(t, first.Id, third.Id)
	assert.Equal(t, 4, third.Delta)

	queued, err := r.GetMatchmakingRequestsForUser(user)
	assert.NoError(t, err)
	assert.Len(t, queued, 1, "exactly one queued row per player")
	assert.Equal(t, first.Id, queued[0].Id)
}

func TestRepository_CreateMatchmakingRequest_distinctUsersAreSeparate(t *testing.T) {
	r := NewRepository(test.DB(), test.PubSub())
	alice := createTestUser(t, r, "alice")
	bob := createTestUser(t, r, "bob")

	reqA, err := r.CreateMatchmakingRequest(alice, 5)
	assert.NoError(t, err)
	reqB, err := r.CreateMatchmakingRequest(bob, 6)
	assert.NoError(t, err)
	assert.NotEqual(t, reqA.Id, reqB.Id)

	queued, err := r.GetMatchmakingRequests()
	assert.NoError(t, err)
	if len(queued) < 2 {
		t.Fatalf("expected at least 2 queued requests for distinct users, got %d", len(queued))
	}
}

func TestRepository_WithTx_rollsBackOnFailure(t *testing.T) {
	r := NewRepository(test.DB(), test.PubSub())
	user := createTestUser(t, r, "rollback")

	req, err := r.CreateMatchmakingRequest(user, 8)
	assert.NoError(t, err)

	gamesBefore, err := r.GetGamesByState([]models.GameState{models.GameStateNegotiation})
	assert.NoError(t, err)

	injected := errors.New("injected failure after game creation")
	err = r.WithTx(func(tx *Repository) error {
		// Create a game and delete the request inside the transaction...
		_, gerr := tx.CreateGame(models.GameTypeStandard, 19, models.GameStateNegotiation, []models.User{user})
		assert.NoError(t, gerr)
		assert.NoError(t, tx.DeleteMatchmakingRequest(*req))
		// ...then fail before commit.
		return injected
	})
	assert.Equal(t, injected, err)

	// The request deletion and the game creation must both have rolled back.
	queued, err := r.GetMatchmakingRequestsForUser(user)
	assert.NoError(t, err)
	assert.Len(t, queued, 1)
	assert.Equal(t, req.Id, queued[0].Id, "request must survive the rolled-back transaction")

	gamesAfter, err := r.GetGamesByState([]models.GameState{models.GameStateNegotiation})
	assert.NoError(t, err)
	assert.Len(t, gamesAfter, len(gamesBefore), "no half-finished game may survive the rollback")
}

func TestRepository_WithTx_commitsOnSuccess(t *testing.T) {
	r := NewRepository(test.DB(), test.PubSub())
	user := createTestUser(t, r, "commit")

	req, err := r.CreateMatchmakingRequest(user, 3)
	assert.NoError(t, err)

	var gameId string
	err = r.WithTx(func(tx *Repository) error {
		game, gerr := tx.CreateGame(models.GameTypeStandard, 19, models.GameStateNegotiation, []models.User{user})
		if gerr != nil {
			return gerr
		}
		gameId = game.Id
		return tx.DeleteMatchmakingRequest(*req)
	})
	assert.NoError(t, err)

	queued, err := r.GetMatchmakingRequestsForUser(user)
	assert.NoError(t, err)
	assert.Empty(t, queued, "request removed by committed transaction")

	game, err := r.GetGameById(gameId)
	assert.NoError(t, err)
	assert.Equal(t, models.GameStateNegotiation, game.State)
}
