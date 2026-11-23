package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alwitt/goutils"
	"github.com/alwitt/livemix/common"
	"github.com/apex/log"
	"github.com/prometheus/client_golang/prometheus"
)

// SegmentReturnCallback function signature of callback to receive a read segment
type SegmentReturnCallback func(ctxt context.Context, segmentID string, content []byte) error

// ErrSegmentReaderStopped is returned when a read is submitted after the reader was stopped
var ErrSegmentReaderStopped = errors.New("segment reader has been stopped")

// SegmentReader support daemon process which reads HLS MPEG-TS segment asynchronously
type SegmentReader interface {
	/*
		ReadSegment read one segment from specified location

			@param ctxt context.Context - execution context
			@param segment common.VideoSegment - video segment
			@param returnCB SegmentReturnCallback - callback used to return the read segment back
	*/
	ReadSegment(
		ctxt context.Context, segment common.VideoSegment, returnCB SegmentReturnCallback,
	) error

	/*
		Stop stops the daemon process

			@param ctxt context.Context - execution context
	*/
	Stop(ctxt context.Context) error
}

// SegmentReaderWithErrorReport is an optional extension of SegmentReader implemented by the
// concrete reader. ReadSegmentWithErrorReport behaves like ReadSegment but additionally reports
// the terminal failure of the read task (source read error, callback error, or shutdown) on the
// returned channel, so callers never have to wait on a callback that will never fire.
type SegmentReaderWithErrorReport interface {
	SegmentReader
	/*
		ReadSegmentWithErrorReport submits a read and returns a channel that receives at most one
		value: the terminal read-task error. A successful read simply leaves the channel empty.

			@param ctxt context.Context - execution context
			@param segment common.VideoSegment - video segment
			@param returnCB SegmentReturnCallback - callback used to return the read segment back
			@returns channel that delivers the terminal read-task error (if any)
	*/
	ReadSegmentWithErrorReport(
		ctxt context.Context, segment common.VideoSegment, returnCB SegmentReturnCallback,
	) (<-chan error, error)
}

// segmentReader implements SegmentReader
type segmentReader struct {
	goutils.Component
	workers          goutils.TaskProcessor
	wg               sync.WaitGroup
	workerContext    context.Context
	workerCtxtCancel context.CancelFunc
	s3               S3Client
	maxSegReadTime   time.Duration

	// stopOnce / stopped ensure Stop is idempotent and reads submitted after shutdown are rejected
	stopOnce sync.Once
	stopped  atomic.Bool

	/* Metrics Collection Agents */
	segmentIOMetrics   SegmentMetricsAgent
	readLatencyMetrics *prometheus.CounterVec
}

// Compile-time guarantees that the concrete reader fulfils both reader contracts.
var (
	_ SegmentReader                = (*segmentReader)(nil)
	_ SegmentReaderWithErrorReport = (*segmentReader)(nil)
)

/*
NewSegmentReader define new SegmentReader

	@param parentContext context.Context - context from which to define the worker context
	@param workerCount int - number of parallel read worker to define
	@param maxSegReadTime time.Duration - max time allowed to completed a segment read
	@param s3 S3Client - S3 client for operating against the S3 server
	@param metrics goutils.MetricsCollector - metrics framework client
	@param tpMetrics goutils.TaskProcessorMetricHelper - task processor metrics helper
	@return new SegmentReader
*/
func NewSegmentReader(
	parentContext context.Context,
	workerCount int,
	maxSegReadTime time.Duration,
	s3 S3Client,
	metrics goutils.MetricsCollector,
	tpMetrics goutils.TaskProcessorMetricHelper,
) (SegmentReader, error) {
	logTags := log.Fields{
		"module":    "utils",
		"component": "hls-video-segment-reader",
	}
	workers, err := goutils.GetNewTaskDemuxProcessorInstance(
		parentContext, "segment-readers", workerCount, workerCount, logTags, tpMetrics,
	)
	if err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to define worker thread pool")
		return nil, err
	}

	workerCtxt, cancel := context.WithCancel(parentContext)

	reader := &segmentReader{
		Component: goutils.Component{
			LogTags: logTags,
			LogTagModifiers: []goutils.LogMetadataModifier{
				goutils.ModifyLogMetadataByRestRequestParam,
			},
		},
		workers:          workers,
		wg:               sync.WaitGroup{},
		workerContext:    workerCtxt,
		workerCtxtCancel: cancel,
		s3:               s3,
		maxSegReadTime:   maxSegReadTime,
		segmentIOMetrics: nil,
	}
	reader.stopped.Store(false)

	// Define supported tasks
	if err := workers.AddToTaskExecutionMap(
		reflect.TypeOf(readSegmentRequest{}), reader.readSegment,
	); err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to install task definition")
		return nil, err
	}

	// Start the workers
	if err := workers.StartEventLoop(&reader.wg); err != nil {
		log.WithError(err).WithFields(logTags).Error("Unable to start the worker thread pool")
		return nil, err
	}

	// Install metrics
	if metrics != nil {
		reader.segmentIOMetrics, err = NewSegmentMetricsAgent(
			parentContext,
			metrics,
			MetricsNameUtilFetcherSegmentLen,
			"Tracking total bytes read by video segment fetcher",
			MetricsNameUtilFetcherIOCount,
			"Tracking total segments read by video segment fetcher",
			[]string{"type", "source"},
		)
		if err != nil {
			log.
				WithError(err).
				WithFields(logTags).
				Error("Unable to define segment IO tracking metrics helper agent")
			return nil, err
		}
		reader.readLatencyMetrics, err = metrics.InstallCustomCounterVecMetrics(
			parentContext,
			MetricsNameUtilFetcherReadLatency,
			"Tracking segment read latency of video segment fetcher",
			[]string{"type"},
		)
		if err != nil {
			log.
				WithError(err).
				WithFields(logTags).
				Error("Unable to define segment read latency tracking metrics")
			return nil, err
		}
	}

	return reader, nil
}

type readSegmentRequest struct {
	segment  common.VideoSegment
	fetchURI *url.URL
	returnCB SegmentReturnCallback
	// taskErr, when set, receives the terminal error of the read task. A successful read leaves
	// it unset. This lets the submitter distinguish "read failed, callback will never fire" from
	// "read still in flight" instead of waiting indefinitely. The channel is buffered so the
	// worker never blocks when no one is listening.
	taskErr chan error
	// reportOnce guards a single terminal failure publication on taskErr.
	reportOnce *sync.Once
}

// reportTaskFailure publishes a terminal read failure on the request's error channel, at most
// once. The user-facing callback is unaffected: it still fires only after a successful read.
func (req readSegmentRequest) reportTaskFailure(reportErr error) {
	if req.taskErr == nil {
		return
	}
	req.reportOnce.Do(func() {
		req.taskErr <- reportErr
	})
}

func (r *segmentReader) ReadSegment(
	ctxt context.Context, segment common.VideoSegment, returnCB SegmentReturnCallback,
) error {
	return r.submitRead(ctxt, segment, returnCB, nil)
}

// submitRead is the internal submission path. When taskErr is non-nil, any terminal failure of
// the read task (source read error, callback error, or shutdown before completion) is reported on
// taskErr exactly once. The user-facing returnCB is still invoked at most once and only after a
// successful source read.
func (r *segmentReader) submitRead(
	ctxt context.Context,
	segment common.VideoSegment,
	returnCB SegmentReturnCallback,
	taskErr chan error,
) error {
	logTags := r.GetLogTagsForContext(ctxt)

	// Reject any reads submitted after the reader was stopped.
	if r.stopped.Load() {
		err := ErrSegmentReaderStopped
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", segment.URI).
			Error("Rejected segment read request: reader stopped")
		return err
	}

	parsedURI, err := url.Parse(segment.URI)
	if err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", segment.URI).
			Error("Unable to parse segment URI")
		return err
	}

	request := readSegmentRequest{
		segment:    segment,
		fetchURI:   parsedURI,
		returnCB:   returnCB,
		taskErr:    taskErr,
		reportOnce: &sync.Once{},
	}

	if err := r.workers.Submit(ctxt, request); err != nil {
		log.WithError(err).WithFields(logTags).Error("Failed to submit 'ReadSegment' job")
		return err
	}

	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segemnt", segment.Name).
		WithField("segment-uri", parsedURI.String()).
		Debug("Submitted 'ReadSegment' job request")

	return nil
}

// ReadSegmentWithErrorReport submits a read and additionally returns a channel that receives the
// terminal read-task error (if any). It is intended for internal callers that need to distinguish
// a failed read (callback never fires) from a still-running one. The returned channel is buffered
// and receives at most one value.
func (r *segmentReader) ReadSegmentWithErrorReport(
	ctxt context.Context, segment common.VideoSegment, returnCB SegmentReturnCallback,
) (<-chan error, error) {
	return r.readSegmentWithErrorReport(ctxt, segment, returnCB)
}

// readSegmentWithErrorReport submits a read and additionally returns a channel that receives the
// terminal read-task error (if any). It is intended for internal callers that need to distinguish
// a failed read (callback never fires) from a still-running one. The returned channel is closed by
// the caller and receives at most one value.
func (r *segmentReader) readSegmentWithErrorReport(
	ctxt context.Context, segment common.VideoSegment, returnCB SegmentReturnCallback,
) (<-chan error, error) {
	taskErr := make(chan error, 1)
	if err := r.submitRead(ctxt, segment, returnCB, taskErr); err != nil {
		return nil, err
	}
	return taskErr, nil
}

func (r *segmentReader) readSegment(params interface{}) error {
	// Convert params into expected data type
	readParams, ok := params.(readSegmentRequest)
	if !ok {
		err := fmt.Errorf("received unexpected call parameters: %s", reflect.TypeOf(params))
		logTags := r.GetLogTagsForContext(r.workerContext)
		log.WithError(err).WithFields(logTags).Error("'readSegment' processing failure")
		return err
	}
	execErr := r.coreReadSegment(
		readParams.segment, readParams.fetchURI, readParams.returnCB,
	)
	if execErr != nil {
		// Surface source-read and callback failures so callers waiting on taskErr fail fast
		// instead of blocking forever. The user callback has not (and will not) fire on this path.
		readParams.reportTaskFailure(execErr)
	}
	return execErr
}

// coreReadSegment contains the actual logic for the ReadSegment function
func (r *segmentReader) coreReadSegment(
	segment common.VideoSegment, fetchURI *url.URL, returnCB SegmentReturnCallback,
) error {
	logTags := r.GetLogTagsForContext(r.workerContext)

	// Choose the read method based on the schema of the segment path URL
	switch fetchURI.Scheme {
	case "file":
		return r.readSegmentFromFile(segment, fetchURI, returnCB)
	case "s3":
		return r.readSegmentFromS3(segment, fetchURI, returnCB)
	default:
		err := fmt.Errorf("invalid segment path URL")
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", segment.String()).
			Error("Unable to read segment file")
		return err
	}
}

// readSegmentFromFile support reading video segment from file
func (r *segmentReader) readSegmentFromFile(
	segment common.VideoSegment, fetchURI *url.URL, returnCB SegmentReturnCallback,
) error {
	logTags := r.GetLogTagsForContext(r.workerContext)

	// Both the file open and the data read are bound by the worker context, so that shutdown
	// or context cancellation cannot leave a read running indefinitely.
	readCtxt, readCtxtCancel := context.WithCancel(r.workerContext)
	defer readCtxtCancel()

	startTime := time.Now().UTC()
	segmentFile, err := os.Open(fetchURI.Path)
	if err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", fetchURI.String()).
			Error("Unable to open segment file")
		return err
	}
	defer func() {
		_ = segmentFile.Close()
	}()

	// Abort a blocked read when the read context ends
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-readCtxt.Done():
			// Closing the file unblocks any in-progress Read call.
			_ = segmentFile.Close()
		case <-stopWatch:
		}
	}()

	type readResult struct {
		content []byte
		err     error
	}
	resultFeed := make(chan readResult, 1)
	go func() {
		content, err := io.ReadAll(segmentFile)
		resultFeed <- readResult{content: content, err: err}
	}()

	var content []byte
	select {
	case <-readCtxt.Done():
		err := fmt.Errorf("segment file read aborted: %w", readCtxt.Err())
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", fetchURI.String()).
			Error("Reading segment file failed")
		return err
	case result := <-resultFeed:
		if result.err != nil {
			log.
				WithError(result.err).
				WithFields(logTags).
				WithField("source-id", segment.SourceID).
				WithField("segemnt", segment.Name).
				WithField("segment-uri", fetchURI.String()).
				Error("Reading segment file failed")
			return result.err
		}
		content = result.content
	}
	endTime := time.Now().UTC()
	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segemnt", segment.Name).
		WithField("segment-uri", fetchURI.String()).
		WithField("length", len(content)).
		Debug("Read segment file")

	// The callback is invoked only after the read succeeded, and any error it returns is
	// propagated back to the caller instead of being swallowed.
	if err := returnCB(readCtxt, segment.ID, content); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", fetchURI.String()).
			Error("Unable to pass on read segment content")
		return err
	}

	// Update metrics
	if r.segmentIOMetrics != nil {
		r.segmentIOMetrics.RecordSegment(len(content), map[string]string{
			"type": "file", "source": segment.SourceID,
		})
	}
	if r.readLatencyMetrics != nil {
		r.readLatencyMetrics.
			With(prometheus.Labels{"type": "file"}).
			Add(float64(endTime.Sub(startTime).Seconds()))
	}
	return nil
}

// CleanupObjectKey cleanup object key string
//
// Remove any leading `/` from object key
func CleanupObjectKey(orig string) string {
	parts := strings.Split(orig, "/")
	keep := []string{}
	for _, onePart := range parts {
		if onePart != "" {
			keep = append(keep, onePart)
		}
	}
	return strings.Join(keep, "/")
}

func (r *segmentReader) readSegmentFromS3(
	segment common.VideoSegment, fetchURI *url.URL, returnCB SegmentReturnCallback,
) error {
	// Bound the S3 read by the configured max read time and the worker lifecycle context.
	// A non-positive max read time means only the worker lifecycle context applies.
	var (
		readCtxt       context.Context
		readCtxtCancel context.CancelFunc
	)
	if r.maxSegReadTime > 0 {
		readCtxt, readCtxtCancel = context.WithTimeout(r.workerContext, r.maxSegReadTime)
	} else {
		readCtxt, readCtxtCancel = context.WithCancel(r.workerContext)
	}
	defer readCtxtCancel()
	logTags := r.GetLogTagsForContext(r.workerContext)

	if r.s3 == nil {
		err := fmt.Errorf("no S3 client specified")
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", fetchURI.String()).
			Error("Unable to to read segment from S3")
		return err
	}

	sourceBucket := fetchURI.Host
	segmentObjectKey := CleanupObjectKey(fetchURI.Path)

	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segemnt", segment.Name).
		WithField("segment-uri", fetchURI.String()).
		Info("Fetching segment from S3")

	startTime := time.Now().UTC()
	content, err := r.s3.GetObject(readCtxt, sourceBucket, segmentObjectKey)
	if err != nil {
		// On timeout or GetObject failure, return the error and never invoke the callback.
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", fetchURI.String()).
			Error("Fetching segment failed")
		return err
	}
	// A context that ended while GetObject was in flight means the content is not trustworthy.
	if err := readCtxt.Err(); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", fetchURI.String()).
			Error("Segment read context ended before completion")
		return err
	}
	endTime := time.Now().UTC()

	log.
		WithFields(logTags).
		WithField("source-id", segment.SourceID).
		WithField("segemnt", segment.Name).
		WithField("segment-uri", fetchURI.String()).
		WithField("length", len(content)).
		Info("Read segment from S3")

	// The callback is invoked only after a successful read, and any error it returns is
	// propagated back to the caller instead of being swallowed.
	if err := returnCB(readCtxt, segment.ID, content); err != nil {
		log.
			WithError(err).
			WithFields(logTags).
			WithField("source-id", segment.SourceID).
			WithField("segemnt", segment.Name).
			WithField("segment-uri", fetchURI.String()).
			Error("Unable to pass on read segment content")
		return err
	}

	// Update metrics
	if r.segmentIOMetrics != nil {
		r.segmentIOMetrics.RecordSegment(len(content), map[string]string{
			"type": "s3", "source": segment.SourceID,
		})
	}
	if r.readLatencyMetrics != nil {
		r.readLatencyMetrics.
			With(prometheus.Labels{"type": "s3"}).
			Add(float64(endTime.Sub(startTime).Seconds()))
	}
	return nil
}

func (r *segmentReader) Stop(ctxt context.Context) error {
	var stopErr error
	r.stopOnce.Do(func() {
		logTags := r.GetLogTagsForContext(ctxt)

		// Mark the reader as stopped first so that any new ReadSegment call is rejected.
		r.stopped.Store(true)

		// Cancel the worker context so that in-flight reads (and their callbacks' contexts)
		// are signalled to stop.
		r.workerCtxtCancel()

		// Stop the worker event loops, then wait until every in-flight task has exited.
		// After this point no worker goroutine is running, therefore no callback can fire.
		if err := r.workers.StopEventLoop(); err != nil {
			log.WithError(err).WithFields(logTags).Error("Failed to stop worker event loops")
			stopErr = err
		}
		if err := goutils.TimeBoundedWaitGroupWait(ctxt, &r.wg, time.Second*10); err != nil {
			log.WithError(err).WithFields(logTags).Error("Timed out waiting for in-flight reads")
			stopErr = err
		}
	})
	return stopErr
}
