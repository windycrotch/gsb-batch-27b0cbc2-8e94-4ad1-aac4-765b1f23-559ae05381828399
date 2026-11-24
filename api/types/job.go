package types

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Job status types.
const (
	JobQueued      = "queued"
	JobDownloading = "downloading"
	JobProbing     = "probing"
	JobEncoding    = "encoding"
	JobUploading   = "uploading"
	JobCompleted   = "completed"
	JobError       = "error"
	JobCancelled   = "cancelled"
	JobRestarting  = "restarting"
)

// JobStatuses All job status types.
var JobStatuses = []string{
	JobQueued,
	JobDownloading,
	JobProbing,
	JobEncoding,
	JobUploading,
	JobCompleted,
	JobError,
	JobCancelled,
	JobRestarting,
}

// Job describes the job info.
type Job struct {
	ID          int64  `db:"id" json:"id"`
	GUID        string `db:"guid" json:"guid"`
	Preset      string `db:"preset" json:"preset"`
	CreatedDate string `db:"created_date" json:"created_date"`
	Status      string `db:"status" json:"status"`
	Source      string `db:"source" json:"source"`
	Destination string `db:"destination" json:"destination"`

	// RunID identifies the execution that currently owns this job. It is
	// NULL while no execution owns the job (queued, or finished). Restarting a
	// job clears it so that late writes from the previous execution are
	// rejected by the store.
	RunID NullString `db:"run_id" json:"-"`

	// CancelRequested is set by the operator cancel endpoint and cleared when a
	// new execution claims the job. It records the intent to cancel even if
	// the status column is overwritten by a concurrent write.
	CancelRequested bool `db:"cancel_requested" json:"-"`

	// EncodeData.
	Encode `db:"encode"`

	LocalSource      string `json:"local_source,omitempty"`
	LocalDestination string `json:"local_destination,omitempty"`
	Streaming        bool   `json:"streaming"`
}

// JobRunState is the minimal view of a job used to detect cancellation,
// terminal states and superseded executions.
type JobRunState struct {
	GUID            string     `db:"guid" json:"guid"`
	Status          string     `db:"status" json:"status"`
	RunID           NullString `db:"run_id" json:"run_id"`
	CancelRequested bool       `db:"cancel_requested" json:"cancel_requested"`
}

// IsTerminalStatus reports whether a job status is final. A terminal status can
// not be changed by progress reports or by another execution.
func IsTerminalStatus(status string) bool {
	switch status {
	case JobCompleted, JobError, JobCancelled:
		return true
	}
	return false
}

// Errors returned while running a job.
var (
	// ErrCancelled is returned when an execution stops because the job was
	// cancelled.
	ErrCancelled = errors.New(JobCancelled)

	// ErrStaleRun is returned when a guarded write is rejected because the job
	// is no longer owned by the calling execution: it was cancelled, finished,
	// or restarted with a new run id.
	ErrStaleRun = errors.New("job run is stale")
)

// Encode describes the encode data.
type Encode struct {
	EncodeID int64       `db:"id" json:"-"`
	JobID    int64       `db:"job_id" json:"-"`
	Probe    NullString  `db:"probe" json:"probe,omitempty"`
	Options  NullString  `db:"options" json:"options,omitempty"`
	Progress NullFloat64 `db:"progress" json:"progress,omitempty"`
	Speed    NullString  `db:"speed" json:"speed"`
	FPS      NullFloat64 `db:"fps" json:"fps"`
}

// NullString is an alias for sql.NullString data type
type NullString struct {
	sql.NullString
}

// MarshalJSON for NullString
func (ns *NullString) MarshalJSON() ([]byte, error) {
	if !ns.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(ns.String)
}

// NullInt64 is an alias for sql.NullInt64 data type
type NullInt64 struct {
	sql.NullInt64
}

// MarshalJSON for NullInt64
func (ni *NullInt64) MarshalJSON() ([]byte, error) {
	if !ni.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(ni.Int64)
}

// NullFloat64 is an alias for sql.NullFloat64 data type
type NullFloat64 struct {
	sql.NullFloat64
}

// MarshalJSON for NullFloat64
func (nf *NullFloat64) MarshalJSON() ([]byte, error) {
	if !nf.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(nf.Float64)
}
