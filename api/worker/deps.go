package worker

import (
	"context"

	"github.com/alfg/openencoder/api/encoder"
	"github.com/alfg/openencoder/api/types"
)

// The worker lifecycle reaches external systems (Postgres, FFmpeg, FFprobe,
// object storage) only through the interfaces below. Production code always uses
// the real implementations defined in this package; tests substitute fakes so
// timing sensitive behaviour can be exercised without external services while
// the production code paths stay under test.

// JobStore is the persistence used by the job lifecycle.
type JobStore interface {
	GetJobRunStateByGUID(guid string) (*types.JobRunState, error)
	ClaimJobRun(guid string, runID string) (*types.JobRunState, bool, error)
	UpdateJobRunStatus(guid string, runID string, status string) error
	UpdateEncodeProbeForRun(guid string, runID string, jsonString string) error
	UpdateEncodeOptionsForRun(guid string, runID string, options string) error
	UpdateEncodeProgressForRun(guid string, runID string, progress float64, speed string, fps float64) error
	UpdateTransferProgressForRun(guid string, runID string, progress float64) error
	FinishJobRun(guid string, runID string, status string) (string, error)
	ReleaseJobRun(guid string, runID string) error
	RequestJobCancelByGUID(guid string) (bool, error)
}

// SettingsStore provides job related settings.
type SettingsStore interface {
	GetSetting(key string) (*types.Setting, error)
	GetSettings() []types.Setting
}

// PresetStore provides preset data.
type PresetStore interface {
	GetPresetByName(name string) (*types.Preset, error)
}

// Encoder runs an encode and exposes cancellable progress. Cancellation is
// driven by the context passed to Run. Done must be called when the execution is
// finished with the encoder so a late cancel becomes a no-op.
type Encoder interface {
	Run(ctx context.Context, input, output, data string) error
	ProgressSnapshot() encoder.Snapshot
	Done()
}

// Prober inspects a media file.
type Prober interface {
	Run(ctx context.Context, input string) (*encoder.FFProbeResponse, error)
}

// Transfer moves files to and from storage.
type Transfer interface {
	Download(ctx context.Context, job types.Job, report func(float64)) error
	Upload(ctx context.Context, job types.Job, report func(float64)) error
	GetPresignedURL(job types.Job) (string, error)
}

// Alerter sends notifications about finished jobs.
type Alerter interface {
	Send(job types.Job) error
}
