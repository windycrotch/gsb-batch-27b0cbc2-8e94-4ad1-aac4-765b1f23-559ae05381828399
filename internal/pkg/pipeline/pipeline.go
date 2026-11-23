package pipeline

import (
	"os"
	"sort"

	"github.com/pkg/errors"
	"github.com/sfluor/musig/internal/pkg/db"
	"github.com/sfluor/musig/internal/pkg/fingerprint"
	"github.com/sfluor/musig/internal/pkg/model"
	"github.com/sfluor/musig/pkg/dsp"
	log "github.com/sirupsen/logrus"
)

// defaultFingerprintBatchSize is the maximum number of fingerprint entries
// persisted in a single database transaction.
const defaultFingerprintBatchSize = 256

// Pipeline is a struct that allows to operate on audio files
type Pipeline struct {
	s   *dsp.Spectrogrammer
	DB  db.Database
	fpr fingerprint.Fingerprinter
	// fingerprintBatchSize bounds the number of fingerprint entries written in
	// a single database transaction. It must stay strictly positive.
	fingerprintBatchSize int
	// failAfterBatches, when positive, makes ProcessAndStore fail after that
	// many fingerprint batches have been committed. Batches committed before
	// the failure remain on disk, which models a crash in the middle of a
	// fingerprint write. A zero value disables the injection. It is intended
	// for testing the retry/idempotency behaviour.
	failAfterBatches int
}

// NewDefaultPipeline creates a new default pipeline using a Bolt DB, the default fingerprinter and the default spectrogrammer
func NewDefaultPipeline(dbFile string) (*Pipeline, error) {
	db, err := db.NewBoltDB(dbFile)
	if err != nil {
		return nil, errors.Wrapf(err, "error connection to database at: %s", dbFile)
	}
	s := dsp.NewSpectrogrammer(model.DownsampleRatio, model.MaxFreq, model.SampleSize, true)
	fpr := fingerprint.NewDefaultFingerprinter()

	return NewPipeline(s, db, fpr), nil
}

// NewPipeline creates a new pipeline
func NewPipeline(s *dsp.Spectrogrammer, db db.Database, fpr fingerprint.Fingerprinter) *Pipeline {
	return &Pipeline{
		s:                    s,
		DB:                   db,
		fpr:                  fpr,
		fingerprintBatchSize: defaultFingerprintBatchSize,
	}
}

// SetFingerprintBatchSize overrides the maximum number of fingerprint entries
// written in a single transaction. Non-positive values are ignored.
func (p *Pipeline) SetFingerprintBatchSize(size int) {
	if size > 0 {
		p.fingerprintBatchSize = size
	}
}

// setFailAfterBatches enables the failure injection used by the tests: the
// next ProcessAndStore call fails after the given number of fingerprint
// batches have been durably committed.
func (p *Pipeline) setFailAfterBatches(batches int) {
	p.failAfterBatches = batches
}

// Close closes the underlying database
func (p *Pipeline) Close() {
	p.DB.Close()
}

// Result represents the output of a pipeline
type Result struct {
	Path         string
	CMap         []model.ConstellationPoint
	SongID       uint32
	SamplingRate float64
	Spectrogram  [][]float64
	Fingerprint  map[model.EncodedKey]model.TableValue
}

// ProcessAndStore process the given audio file and store it in the database
// the computed results are returned.
//
// The whole operation is idempotent: loading the same file any number of times
// keeps a single stable song ID and a single copy of each fingerprint record.
// Fingerprints are persisted in batches inside independent transactions, so a
// failure in the middle of a load only leaves a partial fingerprint set: the
// already committed batches are visible but reloading the same file completes
// the missing records and converges to exactly the same state as a single
// successful load.
func (p *Pipeline) ProcessAndStore(path string) (*Result, error) {
	partial, err := p.read(path)
	if err != nil {
		return nil, err
	}

	id, err := p.DB.SetSong(model.SongNameFromPath(path))
	if err != nil {
		return nil, errors.Wrap(err, "error storing song name in database")
	}

	songFpr := p.fpr.Fingerprint(id, partial.cMap)
	if err := p.storeFingerprint(songFpr); err != nil {
		return nil, errors.Wrap(err, "error storing song fingerprint in database")
	}

	log.Infof("sucessfully loaded %s into the database", path)
	return &Result{
		Path:         path,
		CMap:         partial.cMap,
		SongID:       id,
		SamplingRate: partial.samplingRate,
		Spectrogram:  partial.spectrogram,
		Fingerprint:  songFpr,
	}, nil
}

// storeFingerprint persists the given fingerprint in batches. The database
// layer de-duplicates every (key, value) pair, so retrying after a partial
// failure only fills in the records that are still missing instead of
// appending duplicates.
func (p *Pipeline) storeFingerprint(songFpr map[model.EncodedKey]model.TableValue) error {
	// Sort the keys so that the batches are deterministic: a failure injected
	// after N batches always leaves the same prefix of the fingerprint on disk.
	keys := make([]model.EncodedKey, 0, len(songFpr))
	for k := range songFpr {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	batchSize := p.fingerprintBatchSize
	if batchSize <= 0 {
		batchSize = defaultFingerprintBatchSize
	}

	committedBatches := 0
	for start := 0; start < len(keys); start += batchSize {
		end := start + batchSize
		if end > len(keys) {
			end = len(keys)
		}

		batch := make(map[model.EncodedKey]model.TableValue, end-start)
		for _, k := range keys[start:end] {
			batch[k] = songFpr[k]
		}

		if err := p.DB.Set(batch); err != nil {
			return err
		}
		committedBatches++

		// Test-only failure injection: the batch above is committed, the rest
		// of the fingerprint is still missing, and a retry must fill it in.
		if p.failAfterBatches > 0 && committedBatches >= p.failAfterBatches && end < len(keys) {
			return errors.Errorf("injected failure after %d committed fingerprint batch(es)", committedBatches)
		}
	}

	return nil
}

// Process process the given audio file and returns a Result
func (p *Pipeline) Process(path string) (*Result, error) {
	partial, err := p.read(path)
	if err != nil {
		return nil, err
	}

	// Use 0 as the ID
	var id uint32
	songFpr := p.fpr.Fingerprint(id, partial.cMap)

	log.Infof("sucessfully loaded %s into the database", path)
	return &Result{
		Path:         path,
		CMap:         partial.cMap,
		SongID:       id,
		SamplingRate: partial.samplingRate,
		Spectrogram:  partial.spectrogram,
		Fingerprint:  songFpr,
	}, nil
}

type partialResult struct {
	cMap         []model.ConstellationPoint
	samplingRate float64
	spectrogram  [][]float64
}

func (p *Pipeline) read(path string) (*partialResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrapf(err, "error opening file at %s", path)
	}
	defer file.Close()

	spec, spr, err := p.s.Spectrogram(file)
	if err != nil {
		return nil, errors.Wrap(err, "error generating spectrogram")
	}

	cMap := p.s.ConstellationMap(spec, spr)

	return &partialResult{
		cMap:         cMap,
		samplingRate: spr,
		spectrogram:  spec,
	}, nil
}
