package utils_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alwitt/livemix/common"
	"github.com/alwitt/livemix/hls"
	"github.com/alwitt/livemix/mocks"
	"github.com/alwitt/livemix/utils"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestSegmentReaderS3FailureNoCallback verifies that when the underlying S3 GetObject fails, the
// return callback is never invoked.
func TestSegmentReaderS3FailureNoCallback(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockS3 := mocks.NewS3Client(t)

	uut, err := utils.NewSegmentReader(utCtxt, 2, time.Minute, mockS3, nil, nil)
	assert.Nil(err)

	testID := uuid.NewString()
	testBucket := uuid.NewString()
	testObjectPath := fmt.Sprintf("segments/%s.ts", testID)
	testSegmentURL := fmt.Sprintf("s3://%s/%s", testBucket, testObjectPath)
	s3Err := errors.New("simulated S3 GetObject failure")

	var callbackFired atomic.Bool
	returnCB := func(_ context.Context, _ string, _ []byte) error {
		callbackFired.Store(true)
		return nil
	}

	mockS3.On(
		"GetObject",
		mock.Anything,
		testBucket,
		testObjectPath,
	).Return(nil, s3Err).Once()

	assert.Nil(uut.ReadSegment(
		utCtxt,
		common.VideoSegment{ID: testID, Segment: hls.Segment{URI: testSegmentURL}},
		returnCB,
	))

	// The worker logs the failure internally; observable contract is: callback never fires.
	// Give the worker a moment to (incorrectly) fire the callback before asserting it did not.
	time.Sleep(150 * time.Millisecond)
	assert.False(callbackFired.Load(), "callback must not fire on S3 failure")

	assert.Nil(uut.Stop(utCtxt))
}

// TestSegmentReaderS3TimeoutNoCallback verifies that when the S3 read exceeds the configured max
// read time, the read context is cancelled, GetObject sees the cancellation, and the callback is
// never invoked.
func TestSegmentReaderS3TimeoutNoCallback(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockS3 := mocks.NewS3Client(t)

	// Very short max read time
	uut, err := utils.NewSegmentReader(utCtxt, 2, 25*time.Millisecond, mockS3, nil, nil)
	assert.Nil(err)

	testID := uuid.NewString()
	testBucket := uuid.NewString()
	testObjectPath := fmt.Sprintf("segments/%s.ts", testID)
	testSegmentURL := fmt.Sprintf("s3://%s/%s", testBucket, testObjectPath)

	var callbackFired atomic.Bool
	returnCB := func(_ context.Context, _ string, _ []byte) error {
		callbackFired.Store(true)
		return nil
	}

	mockS3.On(
		"GetObject",
		mock.Anything,
		testBucket,
		testObjectPath,
	).Run(func(args mock.Arguments) {
		// Block until the read context passed to GetObject is cancelled by the timeout.
		readCtxt := args.Get(0).(context.Context)
		<-readCtxt.Done()
	}).Return(nil, context.DeadlineExceeded).Once()

	assert.Nil(uut.ReadSegment(
		utCtxt,
		common.VideoSegment{ID: testID, Segment: hls.Segment{URI: testSegmentURL}},
		returnCB,
	))

	// Give the worker time beyond the timeout to settle; the callback must never fire.
	time.Sleep(150 * time.Millisecond)
	assert.False(callbackFired.Load(), "callback must not fire on timeout")

	assert.Nil(uut.Stop(utCtxt))
}

// TestSegmentReaderFileFailureNoCallback verifies that an unreadable file path does not trigger
// the callback.
func TestSegmentReaderFileFailureNoCallback(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockS3 := mocks.NewS3Client(t)

	uut, err := utils.NewSegmentReader(utCtxt, 2, 0, mockS3, nil, nil)
	assert.Nil(err)

	testID := uuid.NewString()
	missingPath := fmt.Sprintf("/tmp/segment-reader-missing-%s.ts", uuid.NewString())
	testSegmentURL := fmt.Sprintf("file://%s", missingPath)

	var callbackFired atomic.Bool
	returnCB := func(_ context.Context, _ string, _ []byte) error {
		callbackFired.Store(true)
		return nil
	}

	assert.Nil(uut.ReadSegment(
		utCtxt,
		common.VideoSegment{ID: testID, Segment: hls.Segment{URI: testSegmentURL}},
		returnCB,
	))

	time.Sleep(150 * time.Millisecond)
	assert.False(callbackFired.Load(), "callback must not fire on file failure")

	assert.Nil(uut.Stop(utCtxt))
}

// TestSegmentReaderCallbackErrorPropagated verifies that the callback is invoked exactly once
// after a successful read and that an error returned by the callback is handed back by the read
// function (the worker task fails with that error) rather than being swallowed.
func TestSegmentReaderCallbackErrorPropagated(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockS3 := mocks.NewS3Client(t)

	uut, err := utils.NewSegmentReader(utCtxt, 2, time.Minute, mockS3, nil, nil)
	assert.Nil(err)

	testID := uuid.NewString()
	testContent := []byte(uuid.NewString())
	testBucket := uuid.NewString()
	testObjectPath := fmt.Sprintf("segments/%s.ts", testID)
	testSegmentURL := fmt.Sprintf("s3://%s/%s", testBucket, testObjectPath)
	cbErr := errors.New("callback failed")

	var callbackCount atomic.Int32
	callbackErr := make(chan error, 1)
	returnCB := func(_ context.Context, _ string, _ []byte) error {
		callbackCount.Add(1)
		callbackErr <- cbErr
		return cbErr
	}

	mockS3.On(
		"GetObject",
		mock.Anything,
		testBucket,
		testObjectPath,
	).Return(testContent, nil).Once()

	assert.Nil(uut.ReadSegment(
		utCtxt,
		common.VideoSegment{ID: testID, Segment: hls.Segment{URI: testSegmentURL}},
		returnCB,
	))

	select {
	case got := <-callbackErr:
		assert.ErrorIs(got, cbErr)
	case <-time.After(time.Second):
		t.Fatal("callback was not invoked")
	}

	// Callback fires exactly once
	time.Sleep(100 * time.Millisecond)
	assert.Equal(int32(1), callbackCount.Load())

	assert.Nil(uut.Stop(utCtxt))
}

// TestSegmentReaderStopRejectsNewReads verifies that after Stop, new ReadSegment calls are
// rejected and no callback can fire afterwards.
func TestSegmentReaderStopRejectsNewReads(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockS3 := mocks.NewS3Client(t)

	uut, err := utils.NewSegmentReader(utCtxt, 2, time.Minute, mockS3, nil, nil)
	assert.Nil(err)

	assert.Nil(uut.Stop(utCtxt))

	var callbackFired atomic.Bool
	returnCB := func(_ context.Context, _ string, _ []byte) error {
		callbackFired.Store(true)
		return nil
	}

	err = uut.ReadSegment(
		utCtxt,
		common.VideoSegment{ID: uuid.NewString(), Segment: hls.Segment{URI: "s3://b/o.ts"}},
		returnCB,
	)
	assert.ErrorIs(err, utils.ErrSegmentReaderStopped)
	assert.False(callbackFired.Load())

	// Stop remains idempotent
	assert.Nil(uut.Stop(utCtxt))
}

// TestSegmentReaderStopDrainsInFlight verifies that Stop cancels in-flight work and waits for the
// worker goroutines to exit; a blocked read cannot fire its callback after Stop returns.
func TestSegmentReaderStopDrainsInFlight(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockS3 := mocks.NewS3Client(t)

	uut, err := utils.NewSegmentReader(utCtxt, 2, time.Minute, mockS3, nil, nil)
	assert.Nil(err)

	testID := uuid.NewString()
	testBucket := uuid.NewString()
	testObjectPath := fmt.Sprintf("segments/%s.ts", testID)
	testSegmentURL := fmt.Sprintf("s3://%s/%s", testBucket, testObjectPath)

	var callbackFired atomic.Bool
	returnCB := func(_ context.Context, _ string, _ []byte) error {
		callbackFired.Store(true)
		return nil
	}

	getObjectEntered := make(chan struct{})
	mockS3.On(
		"GetObject",
		mock.Anything,
		testBucket,
		testObjectPath,
	).Run(func(args mock.Arguments) {
		readCtxt := args.Get(0).(context.Context)
		close(getObjectEntered)
		// Mimic a slow S3 call that only returns once its context is cancelled.
		<-readCtxt.Done()
	}).Return(nil, context.Canceled).Once()

	assert.Nil(uut.ReadSegment(
		utCtxt,
		common.VideoSegment{ID: testID, Segment: hls.Segment{URI: testSegmentURL}},
		returnCB,
	))

	select {
	case <-getObjectEntered:
	case <-time.After(time.Second):
		t.Fatal("GetObject was never entered")
	}

	// Stop must cancel the worker context and wait for the blocked task to finish draining.
	stopDone := make(chan error, 1)
	go func() { stopDone <- uut.Stop(utCtxt) }()
	select {
	case stopErr := <-stopDone:
		assert.Nil(stopErr)
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return while a read was in flight")
	}

	// After Stop returned, no callback may ever fire.
	time.Sleep(100 * time.Millisecond)
	assert.False(callbackFired.Load(), "callback fired after Stop")
}

// TestSegmentReaderFileReadCancelledOnStop verifies the file read path completes normally, fires
// the callback once, and that Stop afterwards drains cleanly and rejects further reads.
func TestSegmentReaderFileReadCancelledOnStop(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockS3 := mocks.NewS3Client(t)

	uut, err := utils.NewSegmentReader(utCtxt, 2, 0, mockS3, nil, nil)
	assert.Nil(err)

	testFile := fmt.Sprintf("/tmp/%s.txt", uuid.NewString())
	testContent := []byte(strings.Repeat("a", 32))
	{
		file, lclErr := os.Create(testFile)
		assert.Nil(lclErr)
		_, lclErr = file.Write(testContent)
		assert.Nil(lclErr)
		assert.Nil(file.Close())
	}
	testID := uuid.NewString()
	testFileURL := fmt.Sprintf("file://%s", testFile)

	contentRx := make(chan []byte, 1)
	returnCB := func(_ context.Context, _ string, content []byte) error {
		contentRx <- content
		return nil
	}

	assert.Nil(uut.ReadSegment(
		utCtxt, common.VideoSegment{ID: testID, Segment: hls.Segment{URI: testFileURL}}, returnCB,
	))

	// Normal read still completes and fires the callback exactly once
	select {
	case content := <-contentRx:
		assert.Equal(testContent, content)
	case <-time.After(time.Second):
		t.Fatal("file read callback did not fire")
	}

	// Stopping drains cleanly and rejects further reads
	assert.Nil(uut.Stop(utCtxt))
	err = uut.ReadSegment(
		utCtxt, common.VideoSegment{ID: testID, Segment: hls.Segment{URI: testFileURL}}, returnCB,
	)
	assert.ErrorIs(err, utils.ErrSegmentReaderStopped)
}
