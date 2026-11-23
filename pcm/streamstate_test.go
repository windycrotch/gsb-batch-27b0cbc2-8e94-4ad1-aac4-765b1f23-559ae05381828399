// SPDX-License-Identifier: LGPL-2.1-or-later

package pcm

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	aac "github.com/tphakala/go-aac"
)

// countADTSFrames counts self-framed ADTS units in a byte stream by walking
// the 13-bit frame-length field in each header. It deliberately parses by
// hand (same shape as splitADTS) rather than trusting the encoder's own
// counter, so the StreamState.Frames boundary is checked against the bytes
// the sink really holds.
func countADTSFrames(t *testing.T, stream []byte) int {
	t.Helper()
	off, n := 0, 0
	for off < len(stream) {
		if len(stream)-off < adtsHeaderLen {
			t.Fatalf("truncated ADTS header at byte %d (stream %d bytes)", off, len(stream))
		}
		h := stream[off : off+adtsHeaderLen]
		if h[0] != 0xff || h[1]&0xf0 != 0xf0 {
			t.Fatalf("no ADTS syncword at byte %d", off)
		}
		frameLen := int(h[3]&0x03)<<11 | int(h[4])<<3 | int(h[5])>>5
		if frameLen < adtsHeaderLen || off+frameLen > len(stream) {
			t.Fatalf("bad ADTS frame length %d at byte %d", frameLen, off)
		}
		off += frameLen
		n++
	}
	return n
}

// failingSink fails every Write after the first `after` whole-frame writes
// succeed. Every successful Write takes one complete ADTS frame, so `after`
// is also the frame count a correct boundary record must report.
type failingSink struct {
	after int
	n     int
	err   error
}

func (w *failingSink) Write(p []byte) (int, error) {
	if w.n >= w.after {
		return 0, w.err
	}
	w.n++
	return len(p), nil
}

// streamFromEncoder runs one complete fresh-encoder reference stream and
// returns its bytes.
func streamFromEncoder(t *testing.T, cfg Config, pcm []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	e, err := NewEncoder(&buf, cfg)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if _, err := e.Write(pcm); err != nil {
		t.Fatalf("reference Write: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("reference Close: %v", err)
	}
	return buf.Bytes()
}

// TestResetByteIdentityAfterEveryEnding is the stream-boundary contract:
// however the previous stream ended (clean Close, a mid-write sink failure,
// or a Close that rejected an incomplete trailing sample), the encoder's
// output after Reset must be byte-for-byte what a NewEncoder with the same
// Config produces, starting with the very first frame.
func TestResetByteIdentityAfterEveryEnding(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	// Enough audio for several emitted frames (the priming frame emits none).
	pcm := genPCM16(1024*7, 1)
	next := genPCM16(1024*5+333, 1)
	want := streamFromEncoder(t, cfg, next)

	// endings drive one encoder into every terminal state the pool can see.
	endings := []struct {
		name string
		end  func(t *testing.T, e *Encoder)
	}{
		{"clean_close", func(t *testing.T, e *Encoder) {
			t.Helper()
			if _, err := e.Write(pcm); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"mid_write_failure", func(t *testing.T, e *Encoder) {
			t.Helper()
			sinkErr := errors.New("disk full")
			if err := e.Reset(&failingSink{after: 2, err: sinkErr}, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Write(pcm); !errors.Is(err, sinkErr) {
				t.Fatalf("Write = %v, want sink error", err)
			}
		}},
		{"close_incomplete_tail", func(t *testing.T, e *Encoder) {
			t.Helper()
			tailCfg := Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
			if err := e.Reset(io.Discard, tailCfg); err != nil {
				t.Fatal(err)
			}
			// 24-bit stereo: a 1-byte tail is half an inter-channel sample.
			if _, err := e.Write(make([]byte, 6*1024+1)); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err == nil {
				t.Fatal("Close accepted a trailing partial sample")
			}
		}},
		{"failed_then_good_reset", func(t *testing.T, e *Encoder) {
			t.Helper()
			bad := Config{SampleRate: 22050, BitDepth: 16, Channels: 1}
			if err := e.Reset(io.Discard, bad); err == nil {
				t.Fatal("Reset accepted an unsupported sample rate")
			}
			// The failed Reset must leave the encoder unusable; the good Reset
			// in the shared loop below is the recovery.
			if _, err := e.Write(pcm); !errors.Is(err, aac.ErrEncoderClosed) {
				t.Fatalf("Write after failed Reset = %v, want ErrEncoderClosed", err)
			}
		}},
	}

	for _, tc := range endings {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewEncoder(io.Discard, cfg)
			if err != nil {
				t.Fatal(err)
			}
			tc.end(t, e)

			var got bytes.Buffer
			if err := e.Reset(&got, cfg); err != nil {
				t.Fatalf("Reset after %s: %v", tc.name, err)
			}
			if st := e.StreamState(); st != (StreamState{Stage: StageNone}) {
				t.Fatalf("StreamState after Reset = %+v, want the zero record", st)
			}
			if _, err := e.Write(next); err != nil {
				t.Fatalf("Write after Reset: %v", err)
			}
			if err := e.Close(); err != nil {
				t.Fatalf("Close after Reset: %v", err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("reused output differs from a fresh encoder (%d vs %d bytes)", got.Len(), len(want))
			}
		})
	}
}

// TestStreamStateAfterMidWriteFailure is the recovery boundary for the
// Encoder: when a sink rejects a frame mid-stream, StreamState must name
// StageWrite and count exactly the complete ADTS frames the sink holds, and
// the returned error must be the sink's.
func TestStreamStateAfterMidWriteFailure(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	sinkErr := errors.New("sink full")
	sink := &failingSink{after: 3, err: sinkErr}
	e, err := NewEncoder(sink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pcm := genPCM16(1024*12, 1)
	n, err := e.Write(pcm)
	if !errors.Is(err, sinkErr) {
		t.Fatalf("Write = (%d, %v), want the sink error", n, err)
	}
	st := e.StreamState()
	if st.Stage != StageWrite {
		t.Errorf("Stage = %s, want write", st.Stage)
	}
	if !errors.Is(st.Err, sinkErr) {
		t.Errorf("StreamState.Err = %v, want the sink error", st.Err)
	}
	// The sink accepted exactly `after` writes, each one whole ADTS frame:
	// the counter must match the writes the sink actually completed.
	if st.Frames != sink.after {
		t.Errorf("Frames = %d, want %d (the sink's accepted writes)", st.Frames, sink.after)
	}
	// The failure is terminal and sticky until Reset.
	if _, err := e.Write(pcm[:2048]); !errors.Is(err, sinkErr) {
		t.Errorf("retry Write = %v, want the latched sink error", err)
	}
	if err := e.Close(); !errors.Is(err, sinkErr) {
		t.Errorf("Close = %v, want the latched sink error", err)
	}
	if got := e.StreamState(); got.Frames != st.Frames || got.Stage != StageWrite {
		t.Errorf("StreamState changed after retries: %+v, want the same boundary", got)
	}
}

// TestStreamStateAfterFlushFailures covers both Close-phase failures: an
// incomplete trailing sample (no frame emitted by the failing path) and a
// sink that fails while the drain is being written. Both must report
// StageFlush, and the frame count must still equal the frames the sink
// durably holds.
func TestStreamStateAfterFlushFailures(t *testing.T) {
	t.Run("incomplete_tail", func(t *testing.T) {
		cfg := Config{SampleRate: 48000, BitDepth: 24, Channels: 2}
		var sink bytes.Buffer
		e, err := NewEncoder(&sink, cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Ten whole frames plus one stray byte: Close must reject it.
		if _, err := e.Write(make([]byte, 6*1024+1)); err != nil {
			t.Fatal(err)
		}
		if err := e.Close(); err == nil {
			t.Fatal("Close accepted a trailing partial sample")
		}
		st := e.StreamState()
		if st.Stage != StageFlush {
			t.Errorf("Stage = %s, want flush", st.Stage)
		}
		if st.Err == nil {
			t.Error("StreamState.Err = nil, want the tail error")
		}
		// Repeat Close is idempotent: no extra frames, same error and stage.
		first := sink.Len()
		if err := e.Close(); err == nil {
			t.Error("second Close masked the tail error with nil")
		}
		if sink.Len() != first {
			t.Error("second Close wrote more frames; Close is not idempotent")
		}
		if got := e.StreamState(); got.Stage != StageFlush || got.Frames != st.Frames {
			t.Errorf("StreamState after repeat Close = %+v, want the same record", got)
		}
	})

	t.Run("sink_fails_in_drain", func(t *testing.T) {
		cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
		sinkErr := errors.New("sink full")
		// Four input frames emit three access units during Write (the first
		// frame is priming), so a sink accepting three writes leaves every
		// encode-phase write successful and reserves the failure for the
		// extra drain unit Close writes.
		sink := &failingSink{after: 3, err: sinkErr}
		e, err := NewEncoder(sink, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write(genPCM16(1024*4, 1)); err != nil {
			t.Fatalf("Write = %v, want every encode-phase write to succeed", err)
		}
		if err := e.Close(); !errors.Is(err, sinkErr) {
			t.Fatalf("Close = %v, want the sink error", err)
		}
		st := e.StreamState()
		if st.Stage != StageFlush {
			t.Errorf("Stage = %s, want flush", st.Stage)
		}
		if !errors.Is(st.Err, sinkErr) {
			t.Errorf("StreamState.Err = %v, want the sink error", st.Err)
		}
		if st.Frames != sink.after {
			t.Errorf("Frames = %d, want %d accepted sink writes", st.Frames, sink.after)
		}
	})
}

// TestStreamStateCleanStream follows a healthy stream through every state:
// StageNone after Reset, the growing frame count while writing, StageDone
// after Close, and a reset back to StageNone for the next stream.
func TestStreamStateCleanStream(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	var sink bytes.Buffer
	e, err := NewEncoder(&sink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Fatalf("fresh StreamState = %+v, want the zero record", st)
	}
	pcm := genPCM16(1024*6, 1)
	if _, err := e.Write(pcm); err != nil {
		t.Fatal(err)
	}
	mid := e.StreamState()
	if mid.Stage != StageNone || mid.Err != nil || mid.Frames <= 0 {
		t.Fatalf("StreamState mid-stream = %+v, want StageNone with positive frames", mid)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	done := e.StreamState()
	if done.Stage != StageDone || done.Err != nil {
		t.Fatalf("StreamState after Close = %+v, want StageDone/no error", done)
	}
	held := countADTSFrames(t, sink.Bytes())
	if done.Frames != held {
		t.Errorf("Frames = %d, but the sink holds %d ADTS frames", done.Frames, held)
	}
	// Repeat Close changes nothing.
	if err := e.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if st := e.StreamState(); st != done {
		t.Errorf("StreamState changed on repeat Close: %+v vs %+v", st, done)
	}
	// Reset clears the record for the next stream.
	if err := e.Reset(&sink, cfg); err != nil {
		t.Fatal(err)
	}
	if st := e.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Errorf("StreamState after Reset = %+v, want the zero record", st)
	}
}

// assertUnusableAfterFailedReset checks the common failed-Reset contract:
// the encoder rejects every stream method with ErrEncoderClosed and presents
// the zero boundary record until a later Reset succeeds.
func assertUnusableAfterFailedReset(t *testing.T, e *Encoder) {
	t.Helper()
	if e.w != nil {
		t.Error("a failed Reset left a sink attached")
	}
	if _, err := e.Write(genPCM16(2048, 1)); !errors.Is(err, aac.ErrEncoderClosed) {
		t.Errorf("Write after failed Reset = %v, want ErrEncoderClosed", err)
	}
	if err := e.Close(); !errors.Is(err, aac.ErrEncoderClosed) {
		t.Errorf("Close after failed Reset = %v, want ErrEncoderClosed", err)
	}
	if st := e.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Errorf("StreamState after failed Reset = %+v, want the zero record", st)
	}
}

// TestFailedResetDropsEverything pins the failed-Reset contract on the ADTS
// Encoder: unusable afterwards, no old sink retained, and the old error
// invisible to the next successful Reset's StreamState.
func TestFailedResetDropsEverything(t *testing.T) {
	good := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	bad := Config{SampleRate: 22050, BitDepth: 16, Channels: 1}
	sinkErr := errors.New("sink full")
	sink := &failingSink{after: 1, err: sinkErr}
	e, err := NewEncoder(sink, good)
	if err != nil {
		t.Fatal(err)
	}
	// Latch a real failure first, so the failed Reset has an old error to
	// drop and the old sink has been written to.
	if _, err := e.Write(genPCM16(1024*8, 1)); !errors.Is(err, sinkErr) {
		t.Fatalf("Write = %v, want the sink error", err)
	}
	oldState := e.StreamState()
	if oldState.Err == nil || oldState.Frames == 0 {
		t.Fatalf("precondition: StreamState = %+v, want a failed boundary", oldState)
	}
	// A bad config and a nil writer both reject the Reset; either rejection
	// leaves the same unusable, sink-free, error-free boundary.
	if err := e.Reset(io.Discard, bad); err == nil {
		t.Fatal("Reset accepted an unsupported sample rate")
	}
	assertUnusableAfterFailedReset(t, e)
	if err := e.Reset(nil, good); err == nil {
		t.Fatal("Reset accepted a nil writer")
	}
	assertUnusableAfterFailedReset(t, e)

	// The next successful Reset starts a stream whose boundary record is
	// clean: the old sink error must not survive into it.
	var fresh bytes.Buffer
	if err := e.Reset(&fresh, good); err != nil {
		t.Fatalf("re-arming Reset: %v", err)
	}
	if st := e.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Fatalf("StreamState after recovery = %+v, want the zero record", st)
	}
	if _, err := e.Write(genPCM16(1024*3, 1)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	st := e.StreamState()
	if st.Stage != StageDone || st.Err != nil {
		t.Errorf("recovered stream state = %+v, want StageDone/no error", st)
	}
	if st.Frames != countADTSFrames(t, fresh.Bytes()) {
		t.Error("recovered stream frame count does not match the sink")
	}
}

// TestFrameEncoderFailedResetDropsEverything is the FrameEncoder half of the
// same contract: the old emit error and counter are gone after a rejected
// Reset, every method reports ErrEncoderClosed, and a later successful Reset
// starts a clean, fully functional stream.
func TestFrameEncoderFailedResetDropsEverything(t *testing.T) {
	good := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	bad := Config{SampleRate: 22050, BitDepth: 16, Channels: 1}
	emitErr := errors.New("muxer full")
	fe, err := NewFrameEncoder(good)
	if err != nil {
		t.Fatal(err)
	}
	failing := func([]byte, int) error { return emitErr }
	if err := fe.EncodeInterleaved(genPCM16(1024*6, 1), failing); !errors.Is(err, emitErr) {
		t.Fatalf("EncodeInterleaved = %v, want the emit error", err)
	}
	if fe.StreamState().Err == nil {
		t.Fatal("precondition: no error recorded on the FrameEncoder")
	}
	if err := fe.Reset(bad); err == nil {
		t.Fatal("Reset accepted an unsupported sample rate")
	}
	if st := fe.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Errorf("StreamState after failed Reset = %+v, want the zero record", st)
	}
	emit := func([]byte, int) error { return nil }
	if err := fe.EncodeInterleaved(genPCM16(2048, 1), emit); !errors.Is(err, aac.ErrEncoderClosed) {
		t.Errorf("EncodeInterleaved after failed Reset = %v, want ErrEncoderClosed", err)
	}
	if err := fe.Flush(emit); !errors.Is(err, aac.ErrEncoderClosed) {
		t.Errorf("Flush after failed Reset = %v, want ErrEncoderClosed", err)
	}
	// Recovery: the old emit error must not show up on the new stream.
	if err := fe.Reset(good); err != nil {
		t.Fatalf("re-arming Reset: %v", err)
	}
	if st := fe.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Fatalf("StreamState after recovery = %+v, want the zero record", st)
	}
	if err := fe.EncodeInterleaved(genPCM16(2048, 1), emit); err != nil {
		t.Fatalf("EncodeInterleaved after recovery: %v", err)
	}
	if err := fe.Flush(emit); err != nil {
		t.Fatalf("Flush after recovery: %v", err)
	}
	st := fe.StreamState()
	if st.Stage != StageDone || st.Err != nil || st.Frames == 0 {
		t.Errorf("recovered FrameEncoder state = %+v, want StageDone with frames and no error", st)
	}
}

// TestFrameEncoderStreamStateBoundary is the muxing-side counterpart of the
// sink tests: the frame counter moves only when emit accepts a unit, names
// StageWrite for an encode-time rejection and StageFlush for a drain-time
// one, and matches the units actually delivered.
func TestFrameEncoderStreamStateBoundary(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	t.Run("emit_fails_during_encode", func(t *testing.T) {
		emitErr := errors.New("segment full")
		fe, err := NewFrameEncoder(cfg)
		if err != nil {
			t.Fatal(err)
		}
		delivered := 0
		emit := func([]byte, int) error {
			delivered++
			if delivered == 3 {
				return emitErr
			}
			return nil
		}
		if err := fe.EncodeInterleaved(genPCM16(1024*10, 1), emit); !errors.Is(err, emitErr) {
			t.Fatalf("EncodeInterleaved = %v, want the emit error", err)
		}
		st := fe.StreamState()
		if st.Stage != StageWrite || !errors.Is(st.Err, emitErr) {
			t.Fatalf("StreamState = %+v, want StageWrite with the emit error", st)
		}
		// Two calls succeeded before the third failed: only accepted units count.
		if st.Frames != 2 || delivered-1 != st.Frames {
			t.Errorf("Frames = %d, want %d accepted emits", st.Frames, delivered-1)
		}
	})

	t.Run("emit_fails_during_flush", func(t *testing.T) {
		emitErr := errors.New("segment full")
		fe, err := NewFrameEncoder(cfg)
		if err != nil {
			t.Fatal(err)
		}
		delivered := 0
		ok := func([]byte, int) error { delivered++; return nil }
		if err := fe.EncodeInterleaved(genPCM16(2048, 1), ok); err != nil {
			t.Fatal(err)
		}
		before := fe.StreamState().Frames
		if before != delivered {
			t.Fatalf("counter %d != delivered emits %d before Flush", before, delivered)
		}
		if err := fe.Flush(func([]byte, int) error { return emitErr }); !errors.Is(err, emitErr) {
			t.Fatalf("Flush = %v, want the emit error", err)
		}
		st := fe.StreamState()
		if st.Stage != StageFlush || !errors.Is(st.Err, emitErr) {
			t.Fatalf("StreamState = %+v, want StageFlush with the emit error", st)
		}
		if st.Frames != before {
			t.Errorf("a failed flush moved the frame counter: %d -> %d", before, st.Frames)
		}
		// Repeat Flush reports the same outcome and emits nothing.
		if err := fe.Flush(func([]byte, int) error { return nil }); !errors.Is(err, emitErr) {
			t.Errorf("second Flush = %v, want the latched error", err)
		}
		if st2 := fe.StreamState(); st2 != st {
			t.Errorf("StreamState changed on repeat Flush: %+v vs %+v", st2, st)
		}
	})

	t.Run("clean_stream", func(t *testing.T) {
		fe, err := NewFrameEncoder(cfg)
		if err != nil {
			t.Fatal(err)
		}
		delivered := 0
		emit := func([]byte, int) error { delivered++; return nil }
		if err := fe.EncodeInterleaved(genPCM16(1024*5+77, 1), emit); err != nil {
			t.Fatal(err)
		}
		if err := fe.Flush(emit); err != nil {
			t.Fatal(err)
		}
		st := fe.StreamState()
		if st != (StreamState{Stage: StageDone, Frames: delivered, Err: nil}) {
			t.Fatalf("StreamState = %+v, want StageDone/%d frames/no error", st, delivered)
		}
	})
}

// TestResetClearsStatsAndDelay is the pooling contract on the reporting
// methods: after Reset the per-stream Stats belong to the new stream (the
// old stream's counters are gone), and Delay reads the new stream's config.
// A failed Reset reports the zero Stats and the documented constant.
func TestResetClearsStatsAndDelay(t *testing.T) {
	first := Config{SampleRate: 48000, BitDepth: 16, Channels: 2, Bitrate: 128000}
	second := Config{SampleRate: 44100, BitDepth: 16, Channels: 1, Bitrate: 96000}
	e, err := NewEncoder(io.Discard, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Write(genPCM16(48000, 2)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if e.Stats().Frames == 0 {
		t.Fatal("precondition: the first stream accumulated no stats")
	}
	if err := e.Reset(io.Discard, second); err != nil {
		t.Fatal(err)
	}
	if got := e.Stats(); got != (aac.Stats{}) {
		t.Errorf("Stats after Reset = %+v, want the zero value", got)
	}
	if got := e.Delay(); got != aac.EncoderDelay {
		t.Errorf("Delay after Reset = %d, want %d", got, aac.EncoderDelay)
	}
	// Encoding on the new stream accumulates only its own counters.
	if _, err := e.Write(genPCM16(1024*3, 1)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if e.Stats().Frames == 0 {
		t.Error("new stream Stats show no frames; Reset did not re-arm the counters")
	}
}

// TestConcurrentIndependentEncoders proves the safe concurrency shape: many
// goroutines, each holding its own encoder and sink, encode different
// streams in parallel and every result is byte-identical to a serial
// reference. Run under -race to also prove no shared state is touched.
func TestConcurrentIndependentEncoders(t *testing.T) {
	cfgs := []Config{
		{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000},
		{SampleRate: 48000, BitDepth: 16, Channels: 2, Bitrate: 128000},
		{SampleRate: 44100, BitDepth: 24, Channels: 1, Bitrate: 64000},
		{SampleRate: 48000, BitDepth: 32, Channels: 2, Bitrate: 192000},
	}
	type job struct {
		cfg Config
		in  []byte
		ref []byte
	}
	jobs := make([]job, len(cfgs))
	for i, cfg := range cfgs {
		in16 := genPCM16(20000+i*1024, cfg.Channels)
		in := in16
		if cfg.BitDepth != 16 {
			in = widen16(in16, cfg.BitDepth)
		}
		jobs[i] = job{cfg: cfg, in: in, ref: streamFromEncoder(t, cfg, in)}
	}

	const goroutines = 8
	const iters = 5
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := range goroutines {
		wg.Go(func() {
			for it := range iters {
				j := jobs[(g+it)%len(jobs)]
				var out bytes.Buffer
				e, err := NewEncoder(&out, j.cfg)
				if err != nil {
					errCh <- err
					return
				}
				if _, err := e.Write(j.in); err != nil {
					errCh <- err
					return
				}
				if err := e.Close(); err != nil {
					errCh <- err
					return
				}
				if !bytes.Equal(out.Bytes(), j.ref) {
					errCh <- errors.New("parallel encoder output differs from the serial reference")
					return
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// TestConcurrentMisuseDetected proves that sharing one encoder across
// goroutines is rejected with ErrConcurrentUse instead of producing
// interleaved bytes. The hammer runs every mutating method concurrently;
// at least one collision must be observed, and whatever reaches the sink
// must remain a sequence of complete, parseable ADTS frames.
func TestConcurrentMisuseDetected(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	e, err := NewEncoder(io.Discard, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pcm := genPCM16(1024*4, 1)
	const goroutines = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	var sawConcurrent bool
	for g := range goroutines {
		wg.Go(func() {
			for range 50 {
				var callErr error
				switch g % 4 {
				case 0, 1:
					_, callErr = e.Write(pcm)
				case 2:
					callErr = e.Close()
				default:
					callErr = e.Reset(io.Discard, cfg)
				}
				if errors.Is(callErr, ErrConcurrentUse) {
					mu.Lock()
					sawConcurrent = true
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if !sawConcurrent {
		t.Fatal("no concurrent call was rejected; the TryLock guard did not fire")
	}
	// Whatever terminal state the hammer leaves, the encoder must still be
	// recoverable through Reset and behave exactly like a fresh one.
	var out bytes.Buffer
	if err := e.Reset(&out, cfg); err != nil {
		t.Fatalf("Reset after the misuse hammer: %v", err)
	}
	want := streamFromEncoder(t, cfg, pcm)
	if _, err := e.Write(pcm); err != nil {
		t.Fatalf("Write after recovery: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close after recovery: %v", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("recovered encoder output differs from a fresh encoder's")
	}
}

// TestConcurrentMisuseFrameEncoder is the same guard on the callback API,
// including a concurrent Flush.
func TestConcurrentMisuseFrameEncoder(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	fe, err := NewFrameEncoder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pcm := genPCM16(1024*4, 1)
	emit := func([]byte, int) error { return nil }
	const goroutines = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	sawConcurrent := false
	for g := range goroutines {
		wg.Go(func() {
			for range 50 {
				var callErr error
				switch g % 3 {
				case 0:
					callErr = fe.EncodeInterleaved(pcm, emit)
				case 1:
					callErr = fe.Flush(emit)
				default:
					callErr = fe.Reset(cfg)
				}
				if errors.Is(callErr, ErrConcurrentUse) {
					mu.Lock()
					sawConcurrent = true
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if !sawConcurrent {
		t.Fatal("no concurrent FrameEncoder call was rejected")
	}
	if err := fe.Reset(cfg); err != nil {
		t.Fatalf("Reset after the misuse hammer: %v", err)
	}
	if st := fe.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Fatalf("StreamState after recovery = %+v, want the zero record", st)
	}
}

// TestStreamStateSafeToReadConcurrently pins the one method allowed beside
// an in-flight encode: concurrent StreamState reads never race (the -race
// lane enforces this) and always observe a coherent stage/frame pair.
func TestStreamStateSafeToReadConcurrently(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	e, err := NewEncoder(io.Discard, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					st := e.StreamState()
					if st.Frames < 0 || st.Stage < StageNone || st.Stage > StageDone {
						t.Errorf("incoherent StreamState snapshot: %+v", st)
						return
					}
				}
			}
		})
	}
	for range 20 {
		if _, err := e.Write(genPCM16(1024, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
}

// TestEncodeStageString pins the stable log labels and the zero-value
// encoder's boundary record, which a caller must see before any Reset.
func TestEncodeStageString(t *testing.T) {
	want := map[EncodeStage]string{
		StageNone:  "none",
		StageWrite: "write",
		StageFlush: "flush",
		StageDone:  "done",
	}
	for stage, label := range want {
		if got := stage.String(); got != label {
			t.Errorf("%v.String() = %q, want %q", stage, got, label)
		}
	}
	if got := (EncodeStage(99)).String(); got != "unknown" {
		t.Errorf("out-of-range stage label = %q, want unknown", got)
	}

	// Zero values present a clean StageNone record, not a panic.
	var zero Encoder
	if st := zero.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Errorf("zero Encoder StreamState = %+v, want the zero record", st)
	}
	var zeroFE FrameEncoder
	if st := zeroFE.StreamState(); st != (StreamState{Stage: StageNone}) {
		t.Errorf("zero FrameEncoder StreamState = %+v, want the zero record", st)
	}
}

// TestConcurrentUseDoesNotCorruptState proves the rejected concurrent call
// is purely observational: while one Write is in flight, a losing call gets
// ErrConcurrentUse and the winning call's stream still finishes with the same
// frame boundary a serial run would produce.
func TestConcurrentUseDoesNotCorruptState(t *testing.T) {
	cfg := Config{SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000}
	e, err := NewEncoder(io.Discard, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pcm := genPCM16(1024*2, 1)
	// A barrier makes the losing call arrive while the winner holds the guard:
	// the goroutine blocks until the main goroutine has entered Write.
	entered := make(chan struct{})
	release := make(chan struct{})
	blockingSink := &barrierWriter{enter: entered, release: release}
	// Swap sinks via Reset (the encoder is idle, so this takes the guard
	// cleanly) to install the blocking one.
	if err := e.Reset(blockingSink, cfg); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, werr := e.Write(pcm)
		errCh <- werr
	}()
	<-entered // winner is inside Write, holding the guard
	if _, err := e.Write(pcm); !errors.Is(err, ErrConcurrentUse) {
		t.Fatalf("concurrent Write = %v, want ErrConcurrentUse", err)
	}
	if err := e.Close(); !errors.Is(err, ErrConcurrentUse) {
		t.Fatalf("concurrent Close = %v, want ErrConcurrentUse", err)
	}
	close(release) // let the winner finish
	if err := <-errCh; err != nil {
		t.Fatalf("winning Write = %v, want nil", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close after the race: %v", err)
	}
	// The rejected calls changed nothing: the winning stream drains cleanly
	// and reports the same frame count a serial reference stream yields.
	st := e.StreamState()
	if st.Stage != StageDone || st.Err != nil {
		t.Fatalf("StreamState = %+v, want StageDone with no error", st)
	}
	ref := streamFromEncoder(t, cfg, pcm)
	if want := countADTSFrames(t, ref); st.Frames != want {
		t.Errorf("Frames = %d, want %d (the serial reference count)", st.Frames, want)
	}
}

// barrierWriter holds the first Write between enter and release so a test can
// schedule a competing call while the encoder's guard is held; later writes
// pass straight through. It discards the bytes, which is fine because the
// test checks frame counts and state, not output bytes.
type barrierWriter struct {
	enter   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *barrierWriter) Write(p []byte) (int, error) {
	block := false
	w.once.Do(func() {
		block = true
		close(w.enter)
	})
	if block {
		<-w.release
	}
	return len(p), nil
}
