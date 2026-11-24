package matchmake

import (
	"github.com/tengen-io/server/db"
	"github.com/tengen-io/server/models"
	"github.com/tengen-io/server/pubsub"
	"github.com/tengen-io/server/repository"
	"log"
	"os"
	"sort"
	"strconv"
	"time"
)

// pool is the storage abstraction used by the matchmaker. Production code
// backs it with DbPool; tests use an in-memory implementation.
type pool interface {
	// requests returns every matchmaking request currently queued.
	requests() ([]models.MatchmakingRequest, error)
	// enqueue creates a request for the given user. If the user already has
	// an unfinished (still queued) request, that request is returned
	// unchanged instead of inserting a new row, and its rank_delta is
	// preserved.
	enqueue(user models.User, delta int) (*models.MatchmakingRequest, error)
	// match pairs two requests atomically: it creates the negotiation game,
	// deletes both requests and publishes both notifications inside a single
	// transaction. Any failure rolls the whole transaction back, leaving both
	// requests queued for a later run.
	match(models.MatchmakingRequest, models.MatchmakingRequest) error
}

type DbPool struct {
	repo *repository.Repository
}

func (p DbPool) requests() ([]models.MatchmakingRequest, error) {
	return p.repo.GetMatchmakingRequests()
}

func (p DbPool) enqueue(user models.User, delta int) (*models.MatchmakingRequest, error) {
	return p.repo.CreateMatchmakingRequest(user, delta)
}

func (p DbPool) match(i models.MatchmakingRequest, j models.MatchmakingRequest) error {
	log.Printf("matched: %+v, %+v", i, j)
	users := []models.User{
		{NodeFields: models.NodeFields{Id: i.User.Id}}, {NodeFields: models.NodeFields{Id: j.User.Id}},
	}

	// Everything below runs in one transaction. WithTx rolls back on any
	// returned error, so a failure while creating the game, deleting either
	// request or publishing either notification leaves both requests queued
	// and never leaves a half-created game or a single notification behind.
	return p.repo.WithTx(func(r *repository.Repository) error {
		game, err := r.CreateGame(models.GameTypeStandard, 19, models.GameStateNegotiation, users)
		if err != nil {
			return err
		}

		if err := r.DeleteMatchmakingRequest(i); err != nil {
			return err
		}

		if err := r.DeleteMatchmakingRequest(j); err != nil {
			return err
		}

		payload := map[string]interface{}{
			"game": game.Id,
		}
		if err := r.Publish(pubsub.TopicCategoryMatchmakeRequests, pubsub.Event{
			Subject: i.User.Id,
			Event:   "matched",
			Payload: payload,
		}); err != nil {
			return err
		}

		// Fresh map for the second notification so the two events cannot
		// accidentally share mutable state.
		payload = map[string]interface{}{
			"game": game.Id,
		}
		if err := r.Publish(pubsub.TopicCategoryMatchmakeRequests, pubsub.Event{
			Subject: j.User.Id,
			Event:   "matched",
			Payload: payload,
		}); err != nil {
			return err
		}

		return nil
	})
}

type match struct {
	i models.MatchmakingRequest
	j models.MatchmakingRequest
}

type matchmaker struct {
	pool         pool
	tickInterval time.Duration
}

func newMatchmaker(pool pool, tick time.Duration) *matchmaker {
	return &matchmaker{
		pool:         pool,
		tickInterval: tick,
	}
}

func (m *matchmaker) run() {
	// A failed run must never kill the loop: log and continue to the next
	// tick so transient database or notification errors are retried.
	for {
		log.Println("starting matchmaking run...")
		start := time.Now()

		matches, err := m.tick()
		if err != nil {
			log.Printf("error in matchmaking run: %s", err)
		} else {
			log.Printf("matchmaking done. matches: %d", len(matches))

			for _, match := range matches {
				if err := m.pool.match(match.i, match.j); err != nil {
					// The transaction rolled back; both requests stay queued
					// and will be retried on the next run.
					log.Printf("error committing match %s/%s: %s", match.i.Id, match.j.Id, err)
				}
			}
		}

		log.Printf("elapsed: %dms", time.Since(start)/time.Millisecond)
		time.Sleep(m.tickInterval)
	}
}

// tick performs a single scan of the queue. Each player appears in at most
// one pair per scan, a player is never paired with themselves and the same
// pair of players is never produced twice in one scan.
func (m *matchmaker) tick() ([]match, error) {
	requests, err := m.pool.requests()
	if err != nil {
		return nil, err
	}

	removed := make([]bool, len(requests))
	matches := make([]match, 0)

	for i := 0; i < len(requests); i++ {
		if removed[i] {
			continue
		}

		r1 := requests[i]
		// Indices of every still-available, compatible candidate.
		candidates := make([]int, 0)

		for j := 0; j < len(requests); j++ {
			if i == j || removed[j] {
				continue
			}

			r2 := requests[j]

			// A player must never be matched with themselves, even if buggy
			// enqueue logic somehow left more than one queued row for them.
			if r1.User.Id == r2.User.Id {
				continue
			}

			delta := abs(r1.Rank - r2.Rank)
			if delta < r1.Delta && delta < r2.Delta {
				candidates = append(candidates, j)
			}
		}

		if len(candidates) > 0 {
			// Pick the closest-ranked available candidate.
			sort.Slice(candidates, func(a, b int) bool {
				da := abs(r1.Rank - requests[candidates[a]].Rank)
				db := abs(r1.Rank - requests[candidates[b]].Rank)
				return da < db
			})

			partner := candidates[0]

			// Both players are consumed for the rest of this scan, so neither
			// can show up in a second pair.
			removed[i] = true
			removed[partner] = true

			matches = append(matches, match{r1, requests[partner]})
		}
	}

	return matches, nil
}

func Start() {
	repo := makeRepo()
	pool := DbPool{
		repo,
	}

	log.Printf("starting matchmaker")
	tickTimeStr := os.Getenv("TENGEN_MATCHMAKE_TICK_TIME_MS")
	tickTimeMs, err := strconv.Atoi(tickTimeStr)
	tickTime := time.Duration(tickTimeMs) * time.Millisecond

	if err != nil {
		log.Fatalf("Could not parse TENGEN_MATCHMAKE_TICK_TIME_MS")
	}

	matchmaker := newMatchmaker(pool, tickTime)
	matchmaker.run()
}

func abs(n int) int {
	if n < 0 {
		return -1 * n
	}
	return n
}

func makeRepo() *repository.Repository {
	port, err := strconv.Atoi(os.Getenv("TENGEN_DB_PORT"))
	if err != nil {
		log.Fatal("Cold not parse TENGEN_DB_PORT")
	}

	config := db.PostgresDBConfig{
		Host:     os.Getenv("TENGEN_DB_HOST"),
		Port:     port,
		User:     os.Getenv("TENGEN_DB_USER"),
		Database: os.Getenv("TENGEN_DB_DATABASE"),
		Password: os.Getenv("TENGEN_DB_PASSWORD"),
	}

	db, err := db.NewPostgresDb(config)
	ps := pubsub.NewDbPubSub(config.Url())

	if err != nil {
		log.Fatal("Unable to connect to DB.", err)
	}

	return repository.NewRepository(db, ps)
}
