package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/alfg/openencoder/api/data"
	"github.com/alfg/openencoder/api/encoder"
	"github.com/alfg/openencoder/api/net"
	"github.com/alfg/openencoder/api/notify"
	"github.com/alfg/openencoder/api/types"
)

// Defaults used by the job lifecycle. They are variables so tests can shrink
// them to keep timing based tests fast.
var (
	// ProgressInterval is how often encoder progress is persisted.
	ProgressInterval = 5 * time.Second

	// CancelPollInterval bounds how often the lifecycle checks whether the job
	// was cancelled or taken over by another execution. Cancellation is also
	// observed through rejected writes; the poll exists for stages that do not
	// write progress (download, probe, upload) so they are stopped promptly.
	CancelPollInterval = time.Second
)

// Lifecycle runs encode jobs. Every execution is bound to a run id and every
// write it performs is conditional on that id, which gives cancellation,
// restart and normal completion a single convergent lifecycle.
type Lifecycle struct {
	Jobs       JobStore
	Settings   SettingsStore
	Presets    PresetStore
	Transfer   Transfer
	Prober     Prober
	Alerts     Alerter
	WorkDir    string
	NewEncoder func() Encoder

	// ProgressInterval and CancelPollInterval override the package defaults
	// when non-zero.
	ProgressInterval   time.Duration
	CancelPollInterval time.Duration

	// NewRunID overrides run id generation. It is used by tests.
	NewRunID func() string
}

// NewLifecycle builds a lifecycle backed by the production implementations.
func NewLifecycle(workDir string) *Lifecycle {
	db := data.New()
	return &Lifecycle{
		Jobs:     db.Jobs,
		Settings: db.Settings,
		Presets:  db.Presets,
		Transfer: netTransfer{},
		Prober:   ffprobe{},
		Alerts:   slackAlerter{},
		WorkDir:  workDir,
		NewEncoder: func() Encoder {
			return &encoder.FFmpeg{}
		},
	}
}

func (l *Lifecycle) applyDefaults() {
	if l.ProgressInterval == 0 {
		l.ProgressInterval = ProgressInterval
	}
	if l.CancelPollInterval == 0 {
		l.CancelPollInterval = CancelPollInterval
	}
	if l.NewEncoder == nil {
		l.NewEncoder = func() Encoder { return &encoder.FFmpeg{} }
	}
}

func (l *Lifecycle) newRunID() string {
	if l.NewRunID != nil {
		return l.NewRunID()
	}
	return newRunID()
}

// newRunID returns a random 128 bit identifier encoded as hex.
func newRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a
		// timestamp so the process can still make progress.
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

// netTransfer adapts the net package to the Transfer interface.
type netTransfer struct{}

func (netTransfer) Download(ctx context.Context, job types.Job, report func(float64)) error {
	return net.Download(ctx, job, report)
}

func (netTransfer) Upload(ctx context.Context, job types.Job, report func(float64)) error {
	return net.Upload(ctx, job, report)
}

func (netTransfer) GetPresignedURL(job types.Job) (string, error) {
	return net.GetPresignedURL(job)
}

// ffprobe adapts encoder.RunFFProbe to the Prober interface, aborting when the
// context is cancelled.
type ffprobe struct{}

func (ffprobe) Run(ctx context.Context, input string) (*encoder.FFProbeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return encoder.RunFFProbe(ctx, input)
}

// slackAlerter adapts the notify package to the Alerter interface.
type slackAlerter struct{}

func (slackAlerter) Send(job types.Job) error {
	db := data.New()
	webhook, err := db.Settings.GetSetting(types.SlackWebhook)
	if err != nil {
		return err
	}
	if webhook.Value == "" {
		return nil
	}
	message := fmt.Sprintf(AlertMessageFormat, job.GUID, job.Preset, job.Source, job.Destination)
	return notify.SendSlackMessage(webhook.Value, message)
}
