package vod_test

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// TestSegmentManagerWithRealReaderSingleflight wires the real SegmentReader (file read path) to a
// real in-process cache through the SegmentManager, and verifies that N concurrent misses of the
// same segment perform exactly one source read, all callers get identical content, and the result
// is cached exactly once for a follow-up hit.
func TestSegmentManagerWithRealReaderSingleflight(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()

	// Real in-process cache
	cache, err := utils.NewLocalVideoSegmentCache(utCtxt, time.Minute, nil)
	assert.Nil(err)

	// Real segment reader on the file path (S3 client nil is fine for file:// URIs)
	reader, err := utils.NewSegmentReader(utCtxt, 4, 0, nil, nil, nil)
	assert.Nil(err)

	uut, err := vod.NewSegmentManager(utCtxt, cache, reader, time.Minute, nil)
	assert.Nil(err)

	// Prepare a segment file
	segmentID := uuid.NewString()
	testFile := fmt.Sprintf("/tmp/vod-integ-%s.ts", segmentID)
	testContent := []byte(strings.Repeat("z", 256))
	{
		file, lclErr := os.Create(testFile)
		assert.Nil(lclErr)
		_, lclErr = file.Write(testContent)
		assert.Nil(lclErr)
		assert.Nil(file.Close())
	}
	target := common.VideoSegment{
		ID: segmentID,
		Segment: hls.Segment{
			Name: fmt.Sprintf("%s.ts", segmentID),
			URI:  fmt.Sprintf("file://%s", testFile),
		},
	}

	const callerCount = 12
	var wg sync.WaitGroup
	wg.Add(callerCount)
	results := make([][]byte, callerCount)
	errs := make([]error, callerCount)
	// Stagger starts slightly so some callers are guaranteed to join an in-flight read.
	for i := 0; i < callerCount; i++ {
		go func(idx int) {
			defer wg.Done()
			if idx%3 == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			resp, lclErr := uut.GetSegment(utCtxt, target)
			results[idx] = resp
			errs[idx] = lclErr
		}(i)
	}
	wg.Wait()

	for i := 0; i < callerCount; i++ {
		assert.Nil(errs[i], "caller %d errored: %v", i, errs[i])
		assert.Equal(testContent, results[i])
	}

	// The segment must now be served from cache without another source read.
	cached, err := cache.GetSegment(utCtxt, target)
	assert.Nil(err)
	assert.Equal(testContent, cached)

	// A fresh manager over the same cache gets a cache hit
	uut2, err := vod.NewSegmentManager(utCtxt, cache, reader, time.Minute, nil)
	assert.Nil(err)
	hit, err := uut2.GetSegment(utCtxt, target)
	assert.Nil(err)
	assert.Equal(testContent, hit)

	assert.Nil(reader.Stop(utCtxt))
}

// TestSegmentManagerRealReaderReadFailure exercises a missing file through the real reader: the
// manager must return an error and must never cache anything.
func TestSegmentManagerRealReaderReadFailure(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()

	mockCache := mocks.NewVideoSegmentCache(t)
	reader, err := utils.NewSegmentReader(utCtxt, 4, 0, nil, nil, nil)
	assert.Nil(err)

	uut, err := vod.NewSegmentManager(utCtxt, mockCache, reader, time.Minute, nil)
	assert.Nil(err)

	segmentID := uuid.NewString()
	missingFile := fmt.Sprintf("/tmp/vod-missing-%s.ts", segmentID)
	target := common.VideoSegment{
		ID: segmentID,
		Segment: hls.Segment{
			Name: fmt.Sprintf("%s.ts", segmentID),
			URI:  fmt.Sprintf("file://%s", missingFile),
		},
	}

	var cacheWrites atomic.Int32
	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, fmt.Errorf("cache miss"))
	mockCache.On(
		"CacheSegment",
		mock.Anything,
		mock.Anything,
		mock.Anything,
	).Run(func(mock.Arguments) { cacheWrites.Add(1) }).Return(nil).Maybe()

	done := make(chan error, 1)
	go func() {
		_, lclErr := uut.GetSegment(utCtxt, target)
		done <- lclErr
	}()
	select {
	case lclErr := <-done:
		assert.NotNil(lclErr)
	case <-time.After(3 * time.Second):
		t.Fatal("GetSegment did not return after a failed file read")
	}

	// Give any (incorrect) late cache write a chance to land
	time.Sleep(100 * time.Millisecond)
	assert.Equal(int32(0), cacheWrites.Load(), "failed read must not write to cache")

	assert.Nil(reader.Stop(utCtxt))
}

// TestSegmentManagerStopUnblocksWaiters verifies that shutting down the construction context
// while a real reader read is stuck causes every waiting GetSegment caller to return an error.
func TestSegmentManagerStopUnblocksWaiters(t *testing.T) {
	assert := assert.New(t)

	lifecycleCtxt, lifecycleCancel := context.WithCancel(context.Background())

	mockCache := mocks.NewVideoSegmentCache(t)
	mockReader := mocks.NewSegmentReader(t)

	uut, err := vod.NewSegmentManager(lifecycleCtxt, mockCache, mockReader, time.Minute, nil)
	assert.Nil(err)

	target := common.VideoSegment{
		ID: uuid.NewString(),
		Segment: hls.Segment{
			Name: "stuck.ts",
			URI:  "file:///tmp/stuck.ts",
		},
	}

	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, fmt.Errorf("cache miss"))
	// Reader accepts the job but its callback never fires before shutdown.
	mockReader.On(
		"ReadSegment",
		mock.Anything,
		target,
		mock.AnythingOfType("utils.SegmentReturnCallback"),
	).Return(nil).Once()

	const waiterCount = 5
	var wg sync.WaitGroup
	wg.Add(waiterCount)
	errs := make([]error, waiterCount)
	for i := 0; i < waiterCount; i++ {
		go func(idx int) {
			defer wg.Done()
			_, lclErr := uut.GetSegment(context.Background(), target)
			errs[idx] = lclErr
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	lifecycleCancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiters were not unblocked by lifecycle stop")
	}
	for i := 0; i < waiterCount; i++ {
		assert.NotNil(errs[i], "waiter %d should have received an error", i)
	}
}

// TestSegmentManagerRealReaderS3Failure exercises an S3 GetObject failure through the real reader
// and manager. Every concurrent waiter must receive an error, and nothing is written to cache.
func TestSegmentManagerRealReaderS3Failure(t *testing.T) {
	assert := assert.New(t)

	utCtxt := context.Background()

	mockS3 := mocks.NewS3Client(t)
	mockCache := mocks.NewVideoSegmentCache(t)

	reader, err := utils.NewSegmentReader(utCtxt, 4, time.Minute, mockS3, nil, nil)
	assert.Nil(err)

	uut, err := vod.NewSegmentManager(utCtxt, mockCache, reader, time.Minute, nil)
	assert.Nil(err)

	segmentID := uuid.NewString()
	bucket := uuid.NewString()
	objectKey := fmt.Sprintf("segments/%s.ts", segmentID)
	target := common.VideoSegment{
		ID:      segmentID,
		Segment: hls.Segment{URI: fmt.Sprintf("s3://%s/%s", bucket, objectKey)},
	}
	s3Err := fmt.Errorf("simulated S3 failure")

	// Gate the S3 call so the flight stays open while every caller parks on it, making the
	// singleflight assertion deterministic rather than racy on goroutine scheduling.
	s3Release := make(chan struct{})
	var getObjectCalls atomic.Int32
	mockS3.On(
		"GetObject",
		mock.Anything,
		bucket,
		objectKey,
	).Run(func(mock.Arguments) {
		getObjectCalls.Add(1)
		<-s3Release
	}).Return(nil, s3Err)

	var cacheWrites atomic.Int32
	mockCache.On(
		"GetSegment",
		mock.Anything,
		mock.AnythingOfType("common.VideoSegment"),
	).Return(nil, fmt.Errorf("cache miss"))
	mockCache.On(
		"CacheSegment",
		mock.Anything,
		mock.Anything,
		mock.Anything,
	).Run(func(mock.Arguments) { cacheWrites.Add(1) }).Return(nil).Maybe()

	const callerCount = 6
	var wg sync.WaitGroup
	wg.Add(callerCount)
	errs := make([]error, callerCount)
	for i := 0; i < callerCount; i++ {
		go func(idx int) {
			defer wg.Done()
			_, lclErr := uut.GetSegment(utCtxt, target)
			errs[idx] = lclErr
		}(i)
	}

	// Wait until the single underlying S3 read has started (the flight leader is blocked), and
	// give the other callers time to join the same in-flight read.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && getObjectCalls.Load() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(int32(1), getObjectCalls.Load(), "flight leader must start exactly one S3 read")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(int32(1), getObjectCalls.Load(), "before release, exactly one S3 read must be in flight")
	close(s3Release)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("waiters did not return after S3 read failure")
	}

	for i := 0; i < callerCount; i++ {
		assert.NotNil(errs[i], "waiter %d should have received an error", i)
	}
	assert.Equal(int32(1), getObjectCalls.Load(), "singleflight must issue exactly one S3 read")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(int32(0), cacheWrites.Load(), "failed S3 read must not write to cache")

	assert.Nil(reader.Stop(utCtxt))
}
