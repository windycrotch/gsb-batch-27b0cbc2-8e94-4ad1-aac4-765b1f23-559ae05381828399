package vod

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/alwitt/goutils"
	"github.com/alwitt/livemix/common"
	"github.com/alwitt/livemix/utils"
	"github.com/apex/log"
)

// ErrSegmentManagerStopped is returned when the manager has been shut down
var ErrSegmentManagerStopped = errors.New("segment manager has been stopped")

// SegmentManager segment cache management which integrates segment cache with segment reader
type SegmentManager interface {
	/*
		GetSegment fetch video segment from cache. If cache miss, get the segment from source.

			@param ctxt context.Context - execution context
			@param target common.VideoSegment - video segment to fetch
			@returns segment data
	*/
	GetSegment(ctxt context.Context, target common.VideoSegment) ([]byte, error)
}

// inFlightSegment tracks a single source read that may be awaited by multiple callers
type inFlightSegment struct {
	// done is closed once the underlying read finishes, either successfully or with an error
	done chan struct{}
	// finishOnce guarantees the flight is completed (result published + waiters released) exactly
	// once, even if a late callback races with a context-cancel or shutdown completion.
	finishOnce sync.Once
	content    []byte
	err        error
}

// segmentManagerImpl implements SegmentManager
type segmentManagerImpl struct {
	goutils.Component
	cache    utils.VideoSegmentCache
	reader   utils.SegmentReader
	cacheTTL time.Duration

	// lifecycleContext is derived from the construction-time parent context. When the process
	// shuts down (parent context cancelled), every pending GetSegment waiter is released with an
	// error instead of hanging forever.
	lifecycleContext context.Context

	// flightLock guards inFlight, the per-segment singleflight registry
	flightLock sync.Mutex
	inFlight   map[string]*inFlightSegment

	/* Metrics Collection Agents */
	segmentIOMetrics utils.SegmentMetricsAgent
}

/*
NewSegmentManager define new segment manager

	@param ctxt context.Context - execution context
	@param cache utils.VideoSegmentCache - segment cache
	@param reader utils.SegmentReader - segment reader
	@param cacheTTL time.Duration - when recording new entries in cache, use this TTL
	@param metrics goutils.MetricsCollector - metrics framework client
	@returns SegmentManager
*/
func NewSegmentManager(
	ctxt context.Context,
	cache utils.VideoSegmentCache,
	reader utils.SegmentReader,
	cacheTTL time.Duration,
	metrics goutils.MetricsCollector,
) (SegmentManager, error) {
	logTags := log.Fields{"module": "vod", "component": "segment-manager"}

	instance := &segmentManagerImpl{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		cache:            cache,
		reader:           reader,
		cacheTTL:         cacheTTL,
		lifecycleContext: ctxt,
		inFlight:         map[string]*inFlightSegment{},
		segmentIOMetrics: nil,
	}

	// Install metrics
	if metrics != nil {
		var err error
		instance.segmentIOMetrics, err = utils.NewSegmentMetricsAgent(
			ctxt,
			metrics,
			utils.MetricsNameVODSegmentMgmtSegmentLen,
			"Tracking total bytes read VOD segment manager",
			utils.MetricsNameVODSegmentMgmtIOCount,
			"Tracking total segments read by VOD segment manager",
			[]string{"cache_hit", "source"},
		)
		if err != nil {
			log.
				WithError(err).
				WithFields(logTags).
				Error("Unable to define segment IO tracking metrics helper agent")
			return nil, err
		}
	}
	return instance, nil
}

func (m *segmentManagerImpl) GetSegment(
	ctxt context.Context, target common.VideoSegment,
) ([]byte, error) {
	logTags := m.GetLogTagsForContext(ctxt)

	if err := m.lifecycleContext.Err(); err != nil {
		readErr := fmt.Errorf("%w: %s", ErrSegmentManagerStopped, err.Error())
		log.
			WithError(readErr).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			Error("Segment read request rejected: manager stopped")
		return nil, readErr
	}

	// Check cache first
	content, err := m.cache.GetSegment(ctxt, target)
	if err == nil {
		// Update metrics
		if m.segmentIOMetrics != nil {
			m.segmentIOMetrics.RecordSegment(len(content), map[string]string{
				"cache_hit": "true", "source": target.SourceID,
			})
		}
		return content, nil
	}

	// Cache miss
	//
	// If reader was provided, read the segment from the source
	if m.reader == nil {
		err := fmt.Errorf("no segment reader available")
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			Error("Segment read failure")
		return nil, err
	}

	// Singleflight: concurrent cache misses for the same segment join the same in-flight read.
	flight, leader := m.joinOrStartFlight(target)
	if !leader {
		return m.awaitFlight(ctxt, target, flight)
	}
	return m.runFlight(ctxt, target, flight)
}

// joinOrStartFlight either registers this caller as the leader responsible for performing the
// underlying segment read, or returns the already in-flight read that the caller must await.
func (m *segmentManagerImpl) joinOrStartFlight(
	target common.VideoSegment,
) (*inFlightSegment, bool) {
	m.flightLock.Lock()
	defer m.flightLock.Unlock()
	if flight, ok := m.inFlight[target.ID]; ok {
		return flight, false
	}
	flight := &inFlightSegment{done: make(chan struct{})}
	m.inFlight[target.ID] = flight
	return flight, true
}

// finishFlight publishes the read outcome and removes the singleflight entry. Completion happens
// exactly once per flight: any later invocation (e.g. a callback that fires after the caller
// context ended) is ignored, so waiters are released a single time and observe one stable result.
func (m *segmentManagerImpl) finishFlight(
	target common.VideoSegment, flight *inFlightSegment, content []byte, readErr error,
) {
	flight.finishOnce.Do(func() {
		m.flightLock.Lock()
		flight.content = content
		flight.err = readErr
		// Remove the entry only if it still points at this flight.
		if current, ok := m.inFlight[target.ID]; ok && current == flight {
			delete(m.inFlight, target.ID)
		}
		m.flightLock.Unlock()
		close(flight.done)
	})
}

// awaitFlight waits for another caller's in-flight read to complete. Waiters never write to the
// cache and are released with an error if their own context or the manager lifecycle ends first.
func (m *segmentManagerImpl) awaitFlight(
	ctxt context.Context, target common.VideoSegment, flight *inFlightSegment,
) ([]byte, error) {
	logTags := m.GetLogTagsForContext(ctxt)
	select {
	case <-flight.done:
		if flight.err != nil {
			log.
				WithError(flight.err).
				WithFields(logTags).
				WithField("source-id", target.SourceID).
				WithField("segment", target.Name).
				WithField("segment-uri", target.URI).
				Error("Segment read request failed")
			return nil, flight.err
		}
		if m.segmentIOMetrics != nil {
			m.segmentIOMetrics.RecordSegment(len(flight.content), map[string]string{
				"cache_hit": "false", "source": target.SourceID,
			})
		}
		return flight.content, nil
	case <-ctxt.Done():
		err := fmt.Errorf("request context ended before segment completed: %w", ctxt.Err())
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			WithField("segment-uri", target.URI).
			Error("Segment read request failed")
		return nil, err
	case <-m.lifecycleContext.Done():
		err := fmt.Errorf(
			"segment manager stopped before segment completed: %w", m.lifecycleContext.Err(),
		)
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			WithField("segment-uri", target.URI).
			Error("Segment read request failed")
		return nil, err
	}
}

// runFlight performs the underlying source read on behalf of every caller waiting on this flight.
// The read callback publishes at most once; only a fully successful, non-empty result is cached.
// Any failure (read error, empty content, caller-context end, or shutdown) publishes an error and
// leaves the cache untouched.
func (m *segmentManagerImpl) runFlight(
	ctxt context.Context, target common.VideoSegment, flight *inFlightSegment,
) ([]byte, error) {
	logTags := m.GetLogTagsForContext(ctxt)

	// Data return path for the segment reader. The reader invokes the callback inside its worker
	// goroutine; publishing through the flight instead of sending on a channel guarantees the
	// callback never blocks and fires at most once.
	var publishOnce sync.Once
	readSegmentCB := func(_ context.Context, segmentID string, readContent []byte) error {
		publishOnce.Do(func() {
			if len(readContent) == 0 {
				m.finishFlight(
					target, flight, nil,
					fmt.Errorf("segment '%s' read returned empty content", segmentID),
				)
				return
			}
			m.finishFlight(target, flight, readContent, nil)
		})
		return nil
	}

	// Request reading segment from source. When the concrete reader supports error reporting, a
	// source-read or callback failure lands on taskErr so this flight fails fast instead of
	// hanging on a callback that will never fire.
	var (
		taskErr <-chan error
		err     error
	)
	if reportingReader, ok := m.reader.(utils.SegmentReaderWithErrorReport); ok {
		taskErr, err = reportingReader.ReadSegmentWithErrorReport(ctxt, target, readSegmentCB)
	} else {
		err = m.reader.ReadSegment(ctxt, target, readSegmentCB)
	}
	if err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			WithField("segment-uri", target.URI).
			Error("Segment read request could not be submitted")
		m.finishFlight(target, flight, nil, err)
		return nil, err
	}

	// Wait for the outcome. The leader's context ending, the manager shutting down, or the reader
	// reporting a terminal read failure all fail the flight with an error; no content (empty or
	// otherwise) is ever written to the cache.
	//
	// When the reader cannot report task errors, taskErr is nil; a read from a nil channel in a
	// select blocks forever, so that case is simply never selected.
	var content []byte
	select {
	case <-flight.done:
		if flight.err != nil {
			log.
				WithError(flight.err).
				WithFields(logTags).
				WithField("source-id", target.SourceID).
				WithField("segment", target.Name).
				WithField("segment-uri", target.URI).
				Error("Segment read request failed")
			return nil, flight.err
		}
		content = flight.content
	case readFail := <-taskErr:
		// The reader finished the task with an error and never invoked the callback.
		if readFail != nil {
			log.
				WithError(readFail).
				WithFields(logTags).
				WithField("source-id", target.SourceID).
				WithField("segment", target.Name).
				WithField("segment-uri", target.URI).
				Error("Segment source read failed")
			m.finishFlight(target, flight, nil, readFail)
			return nil, readFail
		}
	case <-ctxt.Done():
		err := fmt.Errorf("request context ended before segment completed: %w", ctxt.Err())
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			WithField("segment-uri", target.URI).
			Error("Segment read request failed")
		m.finishFlight(target, flight, nil, err)
		return nil, err
	case <-m.lifecycleContext.Done():
		err := fmt.Errorf(
			"segment manager stopped before segment completed: %w", m.lifecycleContext.Err(),
		)
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			WithField("segment-uri", target.URI).
			Error("Segment read request failed")
		m.finishFlight(target, flight, nil, err)
		return nil, err
	}

	// Record in cache for future reads. This happens only after a complete, successful read and
	// while both the caller context and the manager lifecycle are still active, so failed reads
	// and shutdowns can never leave partial or empty content in the cache.
	if err := m.cache.CacheSegment(
		ctxt, common.VideoSegmentWithData{VideoSegment: target, Content: content}, m.cacheTTL,
	); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", target.SourceID).
			WithField("segment", target.Name).
			Error("Unable to cache newly read segment content")
	}

	// Update metrics
	if m.segmentIOMetrics != nil {
		m.segmentIOMetrics.RecordSegment(len(content), map[string]string{
			"cache_hit": "false", "source": target.SourceID,
		})
	}
	return content, nil
}
