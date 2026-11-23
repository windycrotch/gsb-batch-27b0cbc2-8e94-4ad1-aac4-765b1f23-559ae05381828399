package pcm

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	flac "github.com/tphakala/go-flac"
)

// TestDecoderConcurrentSharedInstance hammers ONE decoder from many goroutines at
// once — the sync.Pool failure mode where two requests hold the same instance
// simultaneously. Read, WriteTo, SeekToSample, Reset and the accessors race each
// other; the contract is that every call is serialized, so no data race exists
// (the -race build validates this) and each call observes one whole stream
// generation, never a half-reset mix. Because every source here is a healthy
// seekable stream, no call may fail: a decode that is restarted by a concurrent
// Reset simply decodes the new stream from its start, and a seek always lands.
// After the storm the instance must still fully recover through one more Reset.
func TestDecoderConcurrentSharedInstance(t *testing.T) {
	names := resetShapeNames
	streams := make(map[string][]byte, len(names))
	infos := make(map[flac.StreamInfo]bool, len(names))
	for _, n := range names {
		s := makeStream(t, resetShapes[n], 9000)
		streams[n] = s
		infos[mustDecoder(t, bytes.NewReader(s)).Info()] = true
	}

	d := mustDecoder(t, bytes.NewReader(streams[names[0]]))

	var wg sync.WaitGroup
	// Resetters rebind the shared decoder to their own stream, over and over.
	for g := range 3 {
		stream := streams[names[g%len(names)]]
		wg.Go(func() {
			for range 15 {
				if err := d.Reset(bytes.NewReader(stream)); err != nil {
					t.Errorf("concurrent Reset: %v", err)
					return
				}
			}
		})
	}
	// Readers drain whatever stream generation they happen to land in. Every
	// generation is a complete valid stream, so the decode itself never errors.
	wg.Go(func() {
		buf := make([]byte, 4096)
		for range 60 {
			if _, err := d.Read(buf); err != nil && !errors.Is(err, io.EOF) {
				t.Errorf("concurrent Read: %v", err)
				return
			}
		}
	})
	wg.Go(func() {
		for range 15 {
			if _, err := d.WriteTo(io.Discard); err != nil {
				t.Errorf("concurrent WriteTo: %v", err)
				return
			}
		}
	})
	// A seeker races the resetters; every stream is seekable and valid, so each
	// seek must succeed on whichever generation it lands.
	wg.Go(func() {
		for range 30 {
			if _, err := d.SeekToSample(4096); err != nil {
				t.Errorf("concurrent SeekToSample: %v", err)
				return
			}
		}
	})
	// An accessor reader must always observe one whole stream's identity, never
	// a torn mix of two generations.
	wg.Go(func() {
		for range 60 {
			if !infos[d.Info()] {
				t.Errorf("Info() = %+v, not any bound stream's STREAMINFO", d.Info())
				return
			}
			if obd := d.OutputBitDepth(); obd != 16 && obd != 24 {
				t.Errorf("OutputBitDepth() = %d, want a bound stream's depth", obd)
				return
			}
			_ = d.Vendor()
			_ = d.Tags()
		}
	})
	wg.Wait()

	// The shared instance must still recover completely through one more Reset.
	good := streams[names[0]]
	if err := d.Reset(bytes.NewReader(good)); err != nil {
		t.Fatalf("post-storm Reset: %v", err)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, d); err != nil {
		t.Fatalf("post-storm decode: %v", err)
	}
	_, want := decodeAll(t, bytes.NewReader(good))
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("post-storm decode differs from fresh: %d vs %d bytes", got.Len(), len(want))
	}
}

// TestDecoderConcurrentPoolReuse exercises the documented sync.Pool pattern with
// more borrowers than instances, so borrowers overlap on shared instances. Each
// borrower resets to its own stream and drains it; per-call serialization means
// a borrower may decode a different generation than it reset to, so only the
// absence of races, panics and unexpected errors is asserted — plus full
// recoverability of every pooled instance at the end.
func TestDecoderConcurrentPoolReuse(t *testing.T) {
	names := resetShapeNames
	streams := make(map[string][]byte, len(names))
	for _, n := range names {
		streams[n] = makeStream(t, resetShapes[n], 9000)
	}

	pool := sync.Pool{New: func() any { return new(Decoder) }}
	var wg sync.WaitGroup
	for g := range 12 {
		stream := streams[names[g%len(names)]]
		wg.Go(func() {
			for range 8 {
				d := pool.Get().(*Decoder)
				if err := d.Reset(bytes.NewReader(stream)); err != nil {
					t.Errorf("pooled Reset: %v", err)
					pool.Put(d)
					return
				}
				// A pooled instance may be reborrowed... here the SAME borrower
				// holds it, but other goroutines' decoders come from the same
				// pool, so instances are reused across shapes concurrently.
				if _, err := io.Copy(io.Discard, d); err != nil {
					t.Errorf("pooled decode: %v", err)
					pool.Put(d)
					return
				}
				pool.Put(d)
			}
		})
	}
	wg.Wait()

	// Every instance the pool hands out now must still decode any shape exactly.
	for _, n := range names {
		d := pool.Get().(*Decoder)
		if err := d.Reset(bytes.NewReader(streams[n])); err != nil {
			t.Fatalf("post-pool Reset(%s): %v", n, err)
		}
		var got bytes.Buffer
		if _, err := io.Copy(&got, d); err != nil {
			t.Fatalf("post-pool decode(%s): %v", n, err)
		}
		_, want := decodeAll(t, bytes.NewReader(streams[n]))
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("post-pool decode(%s) differs from fresh: %d vs %d bytes", n, got.Len(), len(want))
		}
	}
}

// TestDecoderResetOptionsDoNotLeak pins the per-call option contract: a
// WithConvertTo or SkipMD5Verification passed to one Reset (or NewDecoder) must
// not survive the next Reset, and a Reset WITH options must behave exactly like
// a fresh NewDecoder given the same options.
func TestDecoderResetOptionsDoNotLeak(t *testing.T) {
	good := makeStream(t, resetShapes["stereo16"], 9000) // native 16-bit
	badMD5 := corruptMD5(good)

	d, err := NewDecoder(bytes.NewReader(good), WithConvertTo(24), SkipMD5Verification())
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := d.OutputBitDepth(); got != 24 {
		t.Fatalf("OutputBitDepth with WithConvertTo(24) = %d, want 24", got)
	}

	// Reset without options: the conversion and the MD5 skip must both be gone.
	if err := d.Reset(bytes.NewReader(good)); err != nil {
		t.Fatalf("Reset without options: %v", err)
	}
	if got := d.OutputBitDepth(); got != 16 {
		t.Fatalf("OutputBitDepth after plain Reset = %d, want the native 16 (WithConvertTo leaked)", got)
	}
	if err := d.Reset(bytes.NewReader(badMD5)); err != nil {
		t.Fatalf("Reset to corrupt-MD5 stream: %v", err)
	}
	if _, err := io.Copy(io.Discard, d); !errors.Is(err, flac.ErrMD5Mismatch) {
		t.Fatalf("decode after plain Reset: err = %v, want ErrMD5Mismatch (SkipMD5Verification leaked)", err)
	}

	// A Reset WITH the option skips verification again, exactly like a fresh
	// decoder built with it.
	if err := d.Reset(bytes.NewReader(badMD5), SkipMD5Verification()); err != nil {
		t.Fatalf("Reset with SkipMD5Verification: %v", err)
	}
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("decode with SkipMD5Verification: %v", err)
	}

	// Reset with WithConvertTo must match a fresh NewDecoder with the same option.
	for _, depth := range []int{8, 24, 32} {
		if err := d.Reset(bytes.NewReader(good), WithConvertTo(depth)); err != nil {
			t.Fatalf("Reset WithConvertTo(%d): %v", depth, err)
		}
		if got := d.OutputBitDepth(); got != depth {
			t.Fatalf("OutputBitDepth after Reset WithConvertTo(%d) = %d", depth, got)
		}
		var got bytes.Buffer
		if _, err := io.Copy(&got, d); err != nil {
			t.Fatalf("decode WithConvertTo(%d): %v", depth, err)
		}
		fresh, err := NewDecoder(bytes.NewReader(good), WithConvertTo(depth))
		if err != nil {
			t.Fatalf("fresh NewDecoder WithConvertTo(%d): %v", depth, err)
		}
		var want bytes.Buffer
		if _, err := io.Copy(&want, fresh); err != nil {
			t.Fatalf("fresh decode WithConvertTo(%d): %v", depth, err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("Reset WithConvertTo(%d) decode differs from fresh: %d vs %d bytes", depth, got.Len(), want.Len())
		}
	}
}

// TestDecoderResetInvalidConvertPoisonsThenRecovers covers the failed-options
// path: an invalid WithConvertTo depth makes Reset fail and poisons the decoder
// (reads and seeks refuse to serve audio), and the next good Reset fully
// recovers it.
func TestDecoderResetInvalidConvertPoisonsThenRecovers(t *testing.T) {
	good := makeStream(t, resetShapes["stereo16"], 9000)
	d := mustDecoder(t, bytes.NewReader(good))

	rerr := d.Reset(bytes.NewReader(good), WithConvertTo(12))
	if !errors.Is(rerr, flac.ErrUnsupported) {
		t.Fatalf("Reset WithConvertTo(12): err = %v, want ErrUnsupported", rerr)
	}
	if n, err := d.Read(make([]byte, 16)); n != 0 || !errors.Is(err, rerr) {
		t.Fatalf("Read after invalid-convert Reset = (%d, %v), want (0, the Reset error)", n, err)
	}
	if n, err := d.WriteTo(io.Discard); n != 0 || !errors.Is(err, rerr) {
		t.Fatalf("WriteTo after invalid-convert Reset = (%d, %v), want (0, the Reset error)", n, err)
	}
	// Seeking must not revive the decoder or serve audio either.
	if _, err := d.SeekToSample(0); err == nil {
		t.Fatal("SeekToSample after invalid-convert Reset: want error, got nil")
	}
	if got := d.OutputBitDepth(); got != 0 {
		t.Fatalf("OutputBitDepth after invalid-convert Reset = %d, want 0", got)
	}

	// One successful Reset restores full usability.
	if err := d.Reset(bytes.NewReader(good)); err != nil {
		t.Fatalf("recovery Reset: %v", err)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, d); err != nil {
		t.Fatalf("decode after recovery: %v", err)
	}
	_, want := decodeAll(t, bytes.NewReader(good))
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("recovered decode differs from fresh: %d vs %d bytes", got.Len(), len(want))
	}
}

// TestDecoderSeekArgumentErrorsDoNotPoison pins that the argument and capability
// rejections (negative index, non-seekable source) touch no decoder state: the
// decoder keeps decoding the current stream exactly as if the call never happened.
func TestDecoderSeekArgumentErrorsDoNotPoison(t *testing.T) {
	stream := makeStream(t, resetShapes["stereo16"], 9000)
	_, want := decodeAll(t, bytes.NewReader(stream))

	// Non-seekable source: ErrSeekUnsupported, then a clean full decode.
	d := mustDecoder(t, readerOnly{bytes.NewReader(stream)})
	if _, err := d.SeekToSample(100); !errors.Is(err, flac.ErrSeekUnsupported) {
		t.Fatalf("non-seekable SeekToSample: err = %v, want ErrSeekUnsupported", err)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, d); err != nil {
		t.Fatalf("decode after ErrSeekUnsupported: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("decode after ErrSeekUnsupported differs from fresh: %d vs %d bytes", got.Len(), len(want))
	}

	// Seekable source, negative index: ErrInvalidSeek, then a clean full decode.
	d2 := mustDecoder(t, bytes.NewReader(stream))
	if _, err := d2.SeekToSample(-1); !errors.Is(err, flac.ErrInvalidSeek) {
		t.Fatalf("negative SeekToSample: err = %v, want ErrInvalidSeek", err)
	}
	got.Reset()
	if _, err := io.Copy(&got, d2); err != nil {
		t.Fatalf("decode after ErrInvalidSeek: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("decode after ErrInvalidSeek differs from fresh: %d vs %d bytes", got.Len(), len(want))
	}
}

// TestDecoderZeroLengthRead pins the io.Reader zero-length contract: Read with an
// empty buffer decodes nothing and changes nothing, on a healthy decoder and on
// a poisoned one (where it must not surface — or clear — the sticky error).
func TestDecoderZeroLengthRead(t *testing.T) {
	stream := makeStream(t, resetShapes["stereo16"], 9000)
	d := mustDecoder(t, bytes.NewReader(stream))

	for _, p := range [][]byte{nil, {}} {
		if n, err := d.Read(p); n != 0 || err != nil {
			t.Fatalf("zero-length Read = (%d, %v), want (0, nil)", n, err)
		}
	}
	// State untouched: the whole stream is still ahead of the decoder.
	var got bytes.Buffer
	if _, err := io.Copy(&got, d); err != nil {
		t.Fatalf("decode after zero-length reads: %v", err)
	}
	_, want := decodeAll(t, bytes.NewReader(stream))
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("decode after zero-length reads differs from fresh: %d vs %d bytes", got.Len(), len(want))
	}

	// On a poisoned decoder a zero-length read still touches nothing: it neither
	// returns the sticky error nor clears it.
	rerr := d.Reset(bytes.NewReader([]byte("definitely not a flac stream")))
	if rerr == nil {
		t.Fatal("Reset onto garbage: want error, got nil")
	}
	if n, err := d.Read(nil); n != 0 || err != nil {
		t.Fatalf("zero-length Read on poisoned decoder = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := d.Read(make([]byte, 8)); !errors.Is(err, rerr) {
		t.Fatalf("Read on poisoned decoder after zero-length Read: err = %v, want the sticky error %v", err, rerr)
	}
}
