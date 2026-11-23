package vod_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alwitt/livemix/common"
	"github.com/alwitt/livemix/hls"
	"github.com/alwitt/livemix/mocks"
	"github.com/alwitt/livemix/utils"
	"github.com/alwitt/livemix/vod"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// newTestSegment builds a test video segment
func newTestSegment() common.VideoSegment {
	segmentID := uuid.NewString()
	return common.VideoSegment{
		ID: segmentID,
		Segment: hls.Segment{
			Name: fmt.Sprintf("%s.ts", segmentID),
			URI:  fmt.Sprintf("file:///%s.ts", segmentID),
		},
	}
}

// TestSegmentManagerSingleflight verifies that concurrent cache misses for the same segment cause
// at most one underlying source read, and every waiter receives the same content.
func TestSegmentManagerSingleflight(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockCache := mocks.NewVideoSegmentCache(t)
	mockReader := mocks.NewSegmentReader(t)

	uut, err := vod.NewSegmentManager(utCtxt, mockCache, mockReader, time.Minute, nil)
	assert.Nil(err)

	testSegment := newTestSegment()
	content := []byte(uuid.NewString())

	// Cache always misses
	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, errors.New("cache miss"))

	var readCalls atomic.Int32
	// Gate the callback so all goroutines are parked in GetSegment while the read is in flight.
	trigger := make(chan struct{})
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Run(func(args mock.Arguments) {
		readCalls.Add(1)
		returnCB := args.Get(2).(utils.SegmentReturnCallback)
		go func() {
			<-trigger
			assert.Nil(returnCB(context.Background(), testSegment.ID, content))
		}()
	}).Return(nil).Once()

	// Exactly one cache write, happening after the read completes
	cacheWrite := make(chan struct{})
	mockCache.On(
		"CacheSegment",
		mock.Anything,
		mock.MatchedBy(func(seg common.VideoSegmentWithData) bool {
			return seg.ID == testSegment.ID && string(seg.Content) == string(content)
		}),
		mock.AnythingOfType("time.Duration"),
	).Run(func(mock.Arguments) { close(cacheWrite) }).Return(nil).Once()

	const waiterCount = 8
	var wg sync.WaitGroup
	results := make([][]byte, waiterCount)
	errs := make([]error, waiterCount)
	wg.Add(waiterCount)
	for i := 0; i < waiterCount; i++ {
		go func(idx int) {
			defer wg.Done()
			resp, lclErr := uut.GetSegment(utCtxt, testSegment)
			results[idx] = resp
			errs[idx] = lclErr
		}(i)
	}

	// Give every waiter time to park on the in-flight read
	time.Sleep(100 * time.Millisecond)
	assert.Equal(int32(1), readCalls.Load(), "only one underlying read must be submitted")

	close(trigger)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiters did not return after the singleflight read completed")
	}

	assert.Equal(int32(1), readCalls.Load())
	for i := 0; i < waiterCount; i++ {
		assert.Nil(errs[i], "waiter %d got error: %v", i, errs[i])
		assert.Equal(content, results[i])
	}

	// The single cache write must have occurred exactly once
	select {
	case <-cacheWrite:
	case <-time.After(time.Second):
		t.Fatal("expected exactly one CacheSegment call")
	}
}

// TestSegmentManagerReadFailureNotCached verifies that a read whose caller context ends before the
// callback fires surfaces an error to every waiter and never writes anything to the cache, and
// that a later attempt may start a fresh flight instead of reusing the poisoned one.
func TestSegmentManagerReadFailureNotCached(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockCache := mocks.NewVideoSegmentCache(t)
	mockReader := mocks.NewSegmentReader(t)

	uut, err := vod.NewSegmentManager(utCtxt, mockCache, mockReader, time.Minute, nil)
	assert.Nil(err)

	testSegment := newTestSegment()

	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, errors.New("cache miss"))

	// First read: callback never fires while the caller context is alive.
	trigger := make(chan struct{})
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Run(func(args mock.Arguments) {
		returnCB := args.Get(2).(utils.SegmentReturnCallback)
		go func() { <-trigger }()
		go func() {
			// Even if the late callback eventually fires after the flight failed, it must not
			// reach the cache because the flight is already finished.
			<-trigger
			_ = returnCB(context.Background(), testSegment.ID, []byte("late-content"))
		}()
	}).Return(nil).Once()

	// Second attempt after the failed flight should be allowed to start a fresh read.
	successContent := []byte(uuid.NewString())
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Run(func(args mock.Arguments) {
		returnCB := args.Get(2).(utils.SegmentReturnCallback)
		assert.Nil(returnCB(context.Background(), testSegment.ID, successContent))
	}).Return(nil).Once()
	mockCache.On(
		"CacheSegment",
		mock.Anything,
		mock.MatchedBy(func(seg common.VideoSegmentWithData) bool {
			return seg.ID == testSegment.ID && string(seg.Content) == string(successContent)
		}),
		mock.AnythingOfType("time.Duration"),
	).Return(nil).Once()

	// Drive a flight whose read never completes in time; cancel the caller context to force failure.
	lclCtxt, cancel := context.WithCancel(utCtxt)
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
		time.Sleep(50 * time.Millisecond)
		close(trigger)
	}()
	_, err = uut.GetSegment(lclCtxt, testSegment)
	assert.NotNil(err)

	// A subsequent read of the same segment must start a brand new flight (no poisoned entry) and
	// succeed, and only the successful content is ever cached.
	resp, err := uut.GetSegment(utCtxt, testSegment)
	assert.Nil(err)
	assert.Equal(successContent, resp)
}

// TestSegmentManagerSubmitFailureNotCached verifies that a ReadSegment submission error is
// returned to the caller and does not reach the cache.
func TestSegmentManagerSubmitFailureNotCached(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockCache := mocks.NewVideoSegmentCache(t)
	mockReader := mocks.NewSegmentReader(t)

	uut, err := vod.NewSegmentManager(utCtxt, mockCache, mockReader, time.Minute, nil)
	assert.Nil(err)

	testSegment := newTestSegment()
	submitErr := errors.New("reader rejected submission")

	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, errors.New("cache miss"))
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Return(submitErr).Once()

	_, err = uut.GetSegment(utCtxt, testSegment)
	assert.ErrorIs(err, submitErr)

	// A retry starts a fresh flight rather than reusing the failed one.
	content := []byte(uuid.NewString())
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Run(func(args mock.Arguments) {
		returnCB := args.Get(2).(utils.SegmentReturnCallback)
		assert.Nil(returnCB(context.Background(), testSegment.ID, content))
	}).Return(nil).Once()
	mockCache.On(
		"CacheSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegmentWithData"),
		mock.AnythingOfType("time.Duration"),
	).Return(nil).Once()

	resp, err := uut.GetSegment(utCtxt, testSegment)
	assert.Nil(err)
	assert.Equal(content, resp)
}

// TestSegmentManagerEmptyContentNotCached verifies that empty read content is treated as failure
// and never pollutes the cache.
func TestSegmentManagerEmptyContentNotCached(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockCache := mocks.NewVideoSegmentCache(t)
	mockReader := mocks.NewSegmentReader(t)

	uut, err := vod.NewSegmentManager(utCtxt, mockCache, mockReader, time.Minute, nil)
	assert.Nil(err)

	testSegment := newTestSegment()

	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, errors.New("cache miss"))
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Run(func(args mock.Arguments) {
		returnCB := args.Get(2).(utils.SegmentReturnCallback)
		assert.Nil(returnCB(context.Background(), testSegment.ID, []byte{}))
	}).Return(nil).Once()

	_, err = uut.GetSegment(utCtxt, testSegment)
	assert.NotNil(err)
}

// TestSegmentManagerWaiterContextCancel verifies that a waiter whose context ends while the
// shared read is still in flight gets an error and does not block forever.
func TestSegmentManagerWaiterContextCancel(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()
	mockCache := mocks.NewVideoSegmentCache(t)
	mockReader := mocks.NewSegmentReader(t)

	uut, err := vod.NewSegmentManager(utCtxt, mockCache, mockReader, time.Minute, nil)
	assert.Nil(err)

	testSegment := newTestSegment()
	content := []byte(uuid.NewString())

	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, errors.New("cache miss"))

	trigger := make(chan struct{})
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Run(func(args mock.Arguments) {
		returnCB := args.Get(2).(utils.SegmentReturnCallback)
		go func() {
			<-trigger
			assert.Nil(returnCB(context.Background(), testSegment.ID, content))
		}()
	}).Return(nil).Once()
	mockCache.On(
		"CacheSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegmentWithData"),
		mock.AnythingOfType("time.Duration"),
	).Return(nil).Maybe()

	// Leader keeps the flight alive with a long-lived context.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = uut.GetSegment(utCtxt, testSegment)
	}()
	time.Sleep(50 * time.Millisecond)

	// A second caller gives up early.
	waiterCtxt, cancel := context.WithTimeout(utCtxt, 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, waiterErr := uut.GetSegment(waiterCtxt, testSegment)
	elapsed := time.Since(start)
	assert.NotNil(waiterErr)
	assert.Less(elapsed, time.Second)

	// Completing the read unblocks the leader normally.
	close(trigger)
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not return after read completion")
	}
}

// TestSegmentManagerLifecycleStop verifies that when the construction-time context is cancelled
// (service stop), in-flight callers return an error rather than hanging, and subsequent calls are
// rejected.
func TestSegmentManagerLifecycleStop(t *testing.T) {
	assert := assert.New(t)

	lifecycleCtxt, lifecycleCancel := context.WithCancel(context.Background())
	mockCache := mocks.NewVideoSegmentCache(t)
	mockReader := mocks.NewSegmentReader(t)

	uut, err := vod.NewSegmentManager(lifecycleCtxt, mockCache, mockReader, time.Minute, nil)
	assert.Nil(err)

	testSegment := newTestSegment()

	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, errors.New("cache miss"))
	// The read never calls back: simulate a read stuck in flight at shutdown.
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		testSegment,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Return(nil).Once()

	done := make(chan error, 1)
	go func() {
		_, lclErr := uut.GetSegment(context.Background(), testSegment)
		done <- lclErr
	}()
	time.Sleep(100 * time.Millisecond)

	lifecycleCancel()

	select {
	case lclErr := <-done:
		assert.NotNil(lclErr)
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight GetSegment did not return after lifecycle stop")
	}

	// New calls after shutdown are rejected immediately.
	_, err = uut.GetSegment(context.Background(), testSegment)
	assert.ErrorIs(err, vod.ErrSegmentManagerStopped)
}
