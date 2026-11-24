package data

import (
	"database/sql"

	"github.com/alfg/openencoder/api/types"
)

// The operations in this file implement execution scoped writes.
//
// A job can have at most one owning execution at a time, identified by the
// run_id column. Every write made on behalf of an execution carries that run id
// and is a single guarded UPDATE: the guard and the write are one statement, so
// they can not be interleaved with an operator cancel or restart. When the
// guard does not match (the job was cancelled, restarted or already finished)
// the write is rejected with types.ErrStaleRun instead of overwriting the state
// of another execution.

// GetJobRunStateByGUID returns the execution state of a job.
func (j JobsOp) GetJobRunStateByGUID(guid string) (*types.JobRunState, error) {
	const query = `
      SELECT guid, status, run_id, cancel_requested
      FROM jobs
      WHERE guid = $1`

	db, _ := connect()
	defer db.Close()

	state := types.JobRunState{}
	if err := db.Get(&state, query, guid); err != nil {
		log.Error(err)
		return nil, err
	}
	return &state, nil
}

// ClaimJobRun binds an execution to a job by writing its run id.
//
// The claim only succeeds while the job is not terminal, has no owner and has
// no pending cancel request. Because the guard and the write are a single
// statement, two workers racing for the same job can not both win: the loser
// observes the existing owner and does nothing.
func (j JobsOp) ClaimJobRun(guid string, runID string) (*types.JobRunState, bool, error) {
	const claim = `
      UPDATE jobs
      SET run_id = $2, cancel_requested = false
      WHERE guid = $1
        AND run_id IS NULL
        AND NOT cancel_requested
        AND status NOT IN ('cancelled', 'error', 'completed')
      RETURNING guid, status, run_id, cancel_requested`

	db, _ := connect()
	defer db.Close()

	state := types.JobRunState{}
	err := db.Get(&state, claim, guid, runID)
	if err == sql.ErrNoRows {
		// The job exists but is owned by another execution, was cancelled
		// before it started, or is already final.
		current, getErr := j.GetJobRunStateByGUID(guid)
		if getErr != nil {
			return nil, false, getErr
		}
		return current, false, nil
	}
	if err != nil {
		log.Error(err)
		return nil, false, err
	}

	// A new execution starts from a clean slate: the progress of a previous
	// execution must not be inherited.
	const resetProgress = `
      UPDATE encode
      SET progress = 0, speed = NULL, fps = 0
      WHERE job_id = (SELECT id FROM jobs WHERE guid = $1 AND run_id = $2)`
	if _, err := db.Exec(resetProgress, guid, runID); err != nil {
		log.Error(err)
	}

	return &state, true, nil
}

// UpdateJobRunStatus moves a job to a new status on behalf of the execution that
// owns the given run id. It is rejected when the run id does not match or the
// job is already terminal, so a late stage of an old execution can not move a
// finished or restarted job.
func (j JobsOp) UpdateJobRunStatus(guid string, runID string, status string) error {
	const query = `
      UPDATE jobs
      SET status = $3
      WHERE guid = $1
        AND run_id = $2
        AND status NOT IN ('cancelled', 'error', 'completed')`

	db, _ := connect()
	defer db.Close()

	res, err := db.Exec(query, guid, runID, status)
	if err != nil {
		log.Error(err)
		return err
	}
	return checkRowsAffected(res)
}

// UpdateEncodeProbeForRun stores probe data for the owning execution.
func (j JobsOp) UpdateEncodeProbeForRun(guid string, runID string, jsonString string) error {
	const query = `
      UPDATE encode
      SET probe = $3
      WHERE job_id = (SELECT id FROM jobs WHERE guid = $1 AND run_id = $2)
        AND EXISTS (
          SELECT 1 FROM jobs
          WHERE guid = $1 AND run_id = $2
            AND status NOT IN ('cancelled', 'error', 'completed')
        )`

	db, _ := connect()
	defer db.Close()

	res, err := db.Exec(query, guid, runID, jsonString)
	if err != nil {
		log.Error(err)
		return err
	}
	return checkRowsAffected(res)
}

// UpdateEncodeOptionsForRun stores encode options for the owning execution.
func (j JobsOp) UpdateEncodeOptionsForRun(guid string, runID string, options string) error {
	const query = `
      UPDATE encode
      SET options = $3
      WHERE job_id = (SELECT id FROM jobs WHERE guid = $1 AND run_id = $2)
        AND EXISTS (
          SELECT 1 FROM jobs
          WHERE guid = $1 AND run_id = $2
            AND status NOT IN ('cancelled', 'error', 'completed')
        )`

	db, _ := connect()
	defer db.Close()

	res, err := db.Exec(query, guid, runID, options)
	if err != nil {
		log.Error(err)
		return err
	}
	return checkRowsAffected(res)
}

// UpdateEncodeProgressForRun stores encoder progress for the owning execution.
func (j JobsOp) UpdateEncodeProgressForRun(guid string, runID string, progress float64, speed string, fps float64) error {
	const query = `
      UPDATE encode
      SET progress = $3, speed = $4, fps = $5
      WHERE job_id = (SELECT id FROM jobs WHERE guid = $1 AND run_id = $2)
        AND EXISTS (
          SELECT 1 FROM jobs
          WHERE guid = $1 AND run_id = $2
            AND status NOT IN ('cancelled', 'error', 'completed')
        )`

	db, _ := connect()
	defer db.Close()

	res, err := db.Exec(query, guid, runID, progress, speed, fps)
	if err != nil {
		log.Error(err)
		return err
	}
	return checkRowsAffected(res)
}

// UpdateTransferProgressForRun stores transfer progress for the owning
// execution.
func (j JobsOp) UpdateTransferProgressForRun(guid string, runID string, progress float64) error {
	const query = `
      UPDATE encode
      SET progress = $3
      WHERE job_id = (SELECT id FROM jobs WHERE guid = $1 AND run_id = $2)
        AND EXISTS (
          SELECT 1 FROM jobs
          WHERE guid = $1 AND run_id = $2
            AND status NOT IN ('cancelled', 'error', 'completed')
        )`

	db, _ := connect()
	defer db.Close()

	res, err := db.Exec(query, guid, runID, progress)
	if err != nil {
		log.Error(err)
		return err
	}
	return checkRowsAffected(res)
}

// FinishJobRun records the terminal status of an execution and returns the
// status that was actually persisted.
//
// The CASE makes cancellation win over completion: if a cancel request landed
// while the last stage was finishing, the job ends 'cancelled' even when the
// worker reports success. The write only applies while this run still owns the
// job and the job is not already terminal, so exactly one execution records the
// final status.
func (j JobsOp) FinishJobRun(guid string, runID string, status string) (string, error) {
	const query = `
      UPDATE jobs
      SET status = CASE
            WHEN status = 'cancelled' OR cancel_requested THEN 'cancelled'
            ELSE $3
          END,
          run_id = NULL,
          cancel_requested = false
      WHERE guid = $1
        AND run_id = $2
        AND status NOT IN ('cancelled', 'error', 'completed')
      RETURNING status`

	db, _ := connect()
	defer db.Close()

	var final string
	err := db.Get(&final, query, guid, runID, status)
	if err == sql.ErrNoRows {
		// The job is already terminal or owned by another execution: report
		// the status that is actually persisted instead of overwriting it.
		state, getErr := j.GetJobRunStateByGUID(guid)
		if getErr != nil {
			return "", getErr
		}
		return state.Status, types.ErrStaleRun
	}
	if err != nil {
		log.Error(err)
		return "", err
	}
	return final, nil
}

// ReleaseJobRun clears the run binding of an execution that has stopped,
// without changing the job status. It only clears the binding when the job is
// still owned by runID, so it can never un-own a newer execution.
func (j JobsOp) ReleaseJobRun(guid string, runID string) error {
	const query = `
      UPDATE jobs
      SET run_id = NULL, cancel_requested = false
      WHERE guid = $1 AND run_id = $2`

	db, _ := connect()
	defer db.Close()

	if _, err := db.Exec(query, guid, runID); err != nil {
		log.Error(err)
		return err
	}
	return nil
}

// RequestJobCancelByID marks a job for cancellation by ID.
//
// The status is moved to 'cancelled' immediately so the operator sees the
// result, while cancel_requested records the intent for the worker. A job that
// already reached a terminal status is left untouched. The returned bool
// reports whether the job was changed.
func (j JobsOp) RequestJobCancelByID(id int) (bool, error) {
	const query = `
      UPDATE jobs
      SET status = 'cancelled', cancel_requested = true
      WHERE id = $1
        AND status NOT IN ('completed', 'error', 'cancelled')`

	return j.requestCancel(query, id)
}

// RequestJobCancelByGUID marks a job for cancellation by GUID.
func (j JobsOp) RequestJobCancelByGUID(guid string) (bool, error) {
	const query = `
      UPDATE jobs
      SET status = 'cancelled', cancel_requested = true
      WHERE guid = $1
        AND status NOT IN ('completed', 'error', 'cancelled')`

	return j.requestCancel(query, guid)
}

func (j JobsOp) requestCancel(query string, arg interface{}) (bool, error) {
	db, _ := connect()
	defer db.Close()

	res, err := db.Exec(query, arg)
	if err != nil {
		log.Error(err)
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RestartJobRunByGUID prepares a job for a new execution: it clears the run id
// so the next worker execution can claim the job, clears any pending cancel and
// resets progress. The previous execution can no longer write to the job
// because its run id no longer matches. The status moves to 'restarting'.
func (j JobsOp) RestartJobRunByGUID(guid string) (bool, error) {
	const query = `
      UPDATE jobs
      SET status = 'restarting', run_id = NULL, cancel_requested = false
      WHERE guid = $1`

	db, _ := connect()
	defer db.Close()

	res, err := db.Exec(query, guid)
	if err != nil {
		log.Error(err)
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	const progressQuery = `
      UPDATE encode
      SET progress = 0, speed = NULL, fps = 0
      WHERE job_id = (SELECT id FROM jobs WHERE guid = $1)`
	if _, err := db.Exec(progressQuery, guid); err != nil {
		log.Error(err)
	}
	return n > 0, nil
}

// checkRowsAffected converts a no-rows update into types.ErrStaleRun. A guarded
// write that matched nothing means this execution no longer owns the job.
func checkRowsAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return types.ErrStaleRun
	}
	return nil
}
