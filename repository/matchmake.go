package repository

import (
	"database/sql"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/tengen-io/server/models"
	"strconv"
	"time"
)

func (r *Repository) GetMatchmakingRequests() ([]models.MatchmakingRequest, error) {
	rows, err := r.db.Query("SELECT id, user_id, rank, rank_delta, created_at, updated_at FROM matchmake_requests")
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	requests := make([]models.MatchmakingRequest, 0)
	for rows.Next() {
		var request models.MatchmakingRequest
		err := rows.Scan(&request.Id, &request.User.Id, &request.Rank, &request.Delta, &request.CreatedAt, &request.UpdatedAt)
		if err != nil {
			return nil, err
		}

		requests = append(requests, request)
	}

	return requests, nil
}

func (r *Repository) GetMatchmakingRequestsForUser(user models.User) ([]models.MatchmakingRequest, error) {
	rows, err := r.db.Query("SELECT id, user_id, rank, rank_delta, created_at, updated_at FROM matchmake_requests WHERE user_id = $1", user.Id)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	requests := make([]models.MatchmakingRequest, 0)
	for rows.Next() {
		var request models.MatchmakingRequest
		err := rows.Scan(&request.Id, &request.User.Id, &request.Rank, &request.Delta, &request.CreatedAt, &request.UpdatedAt)
		if err != nil {
			return nil, err
		}

		requests = append(requests, request)
	}

	return requests, nil
}

// CreateMatchmakingRequest enqueues a matchmaking request for a user. It is
// idempotent: if the user already has an unfinished request queued (the
// matchmake_requests table enforces a UNIQUE constraint on user_id), the
// existing request is returned unchanged and the submitted delta is ignored,
// so a player clicking "start game" repeatedly ends up with exactly one row
// whose original rank_delta is preserved.
func (r *Repository) CreateMatchmakingRequest(user models.User, delta int) (*models.MatchmakingRequest, error) {
	now := time.Now().UTC()
	ts := pq.FormatTimestamp(now)

	// TODO(eac): add real ranks and queues
	row := r.queryer().QueryRowx("INSERT INTO matchmake_requests (queue, user_id, rank, rank_delta, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id", "FIXME", user.Id, 10, delta, ts, ts)
	var id int64
	err := row.Scan(&id)
	if err != nil {
		// Concurrent (or repeated) submissions hit the UNIQUE (user_id)
		// constraint. Treat that as an idempotent re-enqueue: return the
		// request that is already waiting instead of surfacing an error.
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			existing, getErr := r.getMatchmakingRequestForUser(user)
			if getErr != nil {
				return nil, getErr
			}
			return existing, nil
		}
		return nil, err
	}

	return &models.MatchmakingRequest{
		NodeFields: models.NodeFields{
			Id:        strconv.FormatInt(id, 10),
			CreatedAt: now,
		},
		User:  user,
		Delta: delta,
		Rank:  10,
		Queue: "FIXME",
	}, nil

}

// queryer returns the active transaction when the repository runs inside one,
// otherwise the pooled database handle.
func (r *Repository) queryer() sqlx.Ext {
	if r.tx != nil {
		return r.tx
	}
	return r.db
}

func (r *Repository) getMatchmakingRequestForUser(user models.User) (*models.MatchmakingRequest, error) {
	var request models.MatchmakingRequest
	row := r.queryer().QueryRowx("SELECT id, user_id, rank, rank_delta, created_at, updated_at FROM matchmake_requests WHERE user_id = $1", user.Id)
	if err := row.Scan(&request.Id, &request.User.Id, &request.Rank, &request.Delta, &request.CreatedAt, &request.UpdatedAt); err != nil {
		return nil, err
	}
	return &request, nil
}

// GetMatchmakingRequestForUser returns the single unfinished request queued
// for the given user, or nil when the user is not queued.
func (r *Repository) GetMatchmakingRequestForUser(user models.User) (*models.MatchmakingRequest, error) {
	request, err := r.getMatchmakingRequestForUser(user)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return request, err
}

func (r *Repository) DeleteMatchmakingRequest(request models.MatchmakingRequest) error {
	_, err := r.queryer().Exec("DELETE FROM matchmake_requests WHERE id = $1", request.Id)
	return err
}
