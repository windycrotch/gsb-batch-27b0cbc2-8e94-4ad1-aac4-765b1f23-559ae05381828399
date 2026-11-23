package decoder

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/Eyevinn/mp4ff/avc"

	"github.com/Eyevinn/hi264/internal/cabac"
	"github.com/Eyevinn/hi264/pkg/frame"
)

// permute returns every permutation of the n slice indices.
func permute(n int) [][]int {
	var out [][]int
	a := make([]int, n)
	for i := range n {
		a[i] = i
	}
	var gen func(int)
	gen = func(k int) {
		if k == 1 {
			cp := make([]int, len(a))
			copy(cp, a)
			out = append(out, cp)
			return
		}
		for i := range k {
			a[i], a[k-1] = a[k-1], a[i]
			gen(k - 1)
			a[i], a[k-1] = a[k-1], a[i]
		}
	}
	gen(n)
	return out
}

// A multi-slice IDR picture assembled from its slices in any order, through
// either the batch API or the streaming API, reconstructs the same frame as
// the in-order stream. The decoder may not depend on slice arrival order.
func TestMultiSliceAssemblyOrderInsensitive(t *testing.T) {
	for _, name := range []string{"slices_4", "cavlc_slices_4"} {
		t.Run(name, func(t *testing.T) {
			ps, slices := slicesStream(t, name)
			want, err := New().DecodeNALUs(concatNALUs(ps, slices))
			if err != nil {
				t.Fatalf("in-order reference decode: %v", err)
			}
			wantBytes := want.YUV420Bytes()

			for _, perm := range permute(len(slices)) {
				shuffled := make([][]byte, 0, len(ps)+len(slices))
				shuffled = append(shuffled, ps...)
				for _, i := range perm {
					shuffled = append(shuffled, slices[i])
				}

				// Batch API: DecodeNALUs and DecodeIDRFrames.
				f, err := New().DecodeNALUs(shuffled)
				if err != nil {
					t.Fatalf("DecodeNALUs perm %v: %v", perm, err)
				}
				if !bytes.Equal(f.YUV420Bytes(), wantBytes) {
					t.Errorf("DecodeNALUs perm %v: frame differs from in-order decode", perm)
				}
				frames, err := New().DecodeIDRFrames(shuffled)
				if err != nil || len(frames) != 1 ||
					!bytes.Equal(frames[0].YUV420Bytes(), wantBytes) {
					t.Fatalf("DecodeIDRFrames perm %v: %d frames, err %v", perm, len(frames), err)
				}

				// Streaming API: one NALU at a time, in the same order.
				dec := New()
				var got []byte
				for _, nalu := range shuffled {
					fr, ferr := dec.FeedNALU(nalu)
					if ferr != nil {
						t.Fatalf("FeedNALU perm %v: %v", perm, ferr)
					}
					if fr != nil {
						if got != nil {
							t.Fatalf("perm %v: more than one frame completed", perm)
						}
						got = fr.YUV420Bytes()
					}
				}
				if err := dec.Flush(); err != nil {
					t.Fatalf("Flush perm %v: %v", perm, err)
				}
				if !bytes.Equal(got, wantBytes) {
					t.Errorf("FeedNALU perm %v: frame differs from in-order decode", perm)
				}
			}
		})
	}
}

// A repeated slice is identified and ignored: the picture still reconstructs
// to the in-order frame, the repeated slice adds no coverage, and no error is
// raised. A repeat means a slice whose first_mb_in_slice the current picture
// has already seen, so every case keeps the first arrival of each slice and
// re-sends one of them later — adjacent to its original, after all other
// slices, or interleaved into an out-of-order delivery. This holds through
// both the batch and streaming APIs and for both entropy coders.
func TestMultiSliceDuplicatesIgnored(t *testing.T) {
	for _, name := range []string{"slices_4", "cavlc_slices_4"} {
		t.Run(name, func(t *testing.T) {
			ps, slices := slicesStream(t, name)
			want, err := New().DecodeNALUs(concatNALUs(ps, slices))
			if err != nil {
				t.Fatalf("reference decode: %v", err)
			}
			wantBytes := want.YUV420Bytes()

			for dupIdx := range slices {
				// Repeat right after the original, while the picture is still
				// being assembled: the canonical retransmission pattern.
				assertOneEqualFrame(t, ps, orderedWithRepeat(slices, dupIdx, dupIdx),
					wantBytes, fmt.Sprintf("adjacent dup slice %d", dupIdx))
			}

			// A later slice re-sent after all the other slices (its original
			// was already seen), including an interleaved out-of-order repeat.
			for dupIdx := 1; dupIdx < len(slices); dupIdx++ {
				atEnd := append(append([][]byte{}, slices...), slices[dupIdx])
				assertOneEqualFrame(t, ps, atEnd, wantBytes,
					fmt.Sprintf("end repeat of slice %d", dupIdx))
			}

			// A non-first slice delivered before its position and then again
			// in its original position: the second copy is the repeat.
			for dupIdx := 1; dupIdx < len(slices); dupIdx++ {
				early := append([][]byte{slices[dupIdx]}, slices...)
				assertOneEqualFrame(t, ps, early, wantBytes,
					fmt.Sprintf("early then in-order slice %d", dupIdx))
			}

			// Out-of-order delivery with an interleaved repeat.
			mixed := [][]byte{slices[0], slices[2], slices[1], slices[2], slices[3]}
			assertOneEqualFrame(t, ps, mixed, wantBytes, "shuffled with interleaved repeat")

			// Repeating every slice still yields exactly one frame.
			var doubled [][]byte
			for _, sl := range slices {
				doubled = append(doubled, sl, sl)
			}
			assertOneEqualFrame(t, ps, doubled, wantBytes, "every slice doubled")
		})
	}
}

// assertOneEqualFrame decodes ps+slices through both APIs, requiring exactly
// one frame whose pixels equal wantBytes and no error, including on Flush.
func assertOneEqualFrame(t *testing.T, ps, sl [][]byte, wantBytes []byte, what string) {
	t.Helper()
	nalus := concatNALUs(ps, sl)

	f, err := New().DecodeNALUs(nalus)
	if err != nil {
		t.Fatalf("%s: DecodeNALUs error: %v", what, err)
	}
	if !bytes.Equal(f.YUV420Bytes(), wantBytes) {
		t.Errorf("%s: batch frame differs", what)
	}

	dec := New()
	var frames int
	for _, nalu := range nalus {
		fr, ferr := dec.FeedNALU(nalu)
		if ferr != nil {
			t.Fatalf("%s: FeedNALU error: %v", what, ferr)
		}
		if fr != nil {
			frames++
			if !bytes.Equal(fr.YUV420Bytes(), wantBytes) {
				t.Errorf("%s: streamed frame differs", what)
			}
		}
	}
	if err := dec.Flush(); err != nil {
		t.Fatalf("%s: Flush: %v", what, err)
	}
	if frames != 1 {
		t.Errorf("%s: completed %d frames, want 1", what, frames)
	}
}

// orderedWithRepeat returns slices in order with an extra copy of repeat
// inserted at the position of anchor.
func orderedWithRepeat(slices [][]byte, anchor, repeat int) [][]byte {
	var out [][]byte
	for i, sl := range slices {
		out = append(out, sl)
		if i == anchor {
			out = append(out, slices[repeat])
		}
	}
	return out
}

// A truncated slice must make the decode fail with an error and must never
// produce a frame with uncovered macroblocks: neither on the failing call nor
// on Flush. Every prefix of every slice that contains at least one macroblock
// is exercised, for both entropy coders.
func TestMultiSliceTruncationErrors(t *testing.T) {
	for _, name := range []string{"slices_4", "cavlc_slices_4"} {
		t.Run(name, func(t *testing.T) {
			ps, slices := slicesStream(t, name)
			isCABAC := name == "slices_4"
			sawEndOfData := false
			for si, full := range slices {
				// The NAL header and slice header must remain intact for the
				// failure to happen at slice-data level; cutting inside the
				// header is covered separately by corrupt-header tests.
				headerLen := int(sliceHeaderSize(t, ps, full))
				if headerLen >= len(full) {
					t.Fatalf("slice %d: header of %d bytes leaves no slice data", si, headerLen)
				}
				for n := headerLen + 1; n < len(full); n++ {
					nalus := concatNALUs(ps, replaceSlice(slices, si, full[:n]))
					dec := New()
					var completed int
					var failErr error
					for _, nalu := range nalus {
						f, err := dec.FeedNALU(nalu)
						if err != nil {
							failErr = err
							break
						}
						if f != nil {
							completed++
						}
					}
					if failErr == nil {
						failErr = dec.Flush()
					}
					if failErr == nil {
						t.Errorf("slice %d cut to %d/%d bytes: no error", si, n, len(full))
					}
					if completed != 0 {
						t.Errorf("slice %d cut to %d/%d bytes: completed %d frames, want 0",
							si, n, len(full), completed)
					}
					if isCABAC && errors.Is(failErr, cabac.ErrEndOfData) {
						sawEndOfData = true
					}
				}
			}
			if isCABAC && !sawEndOfData {
				t.Error("no CABAC truncation was reported as cabac.ErrEndOfData")
			}
		})
	}
}

// A corrupted slice header likewise fails the decode without a frame, and the
// decoder stays usable for the next picture.
func TestMultiSliceCorruptHeaderErrors(t *testing.T) {
	ps, slices := slicesStream(t, "slices_4")
	corrupt := append([]byte(nil), slices[2]...)
	corrupt[0] = 0 // forbidden_zero_byte NAL type 0; the header cannot parse
	nalus := concatNALUs(ps, replaceSlice(slices, 2, corrupt))
	dec := New()
	var frames int
	var err error
	for _, nalu := range nalus {
		f, ferr := dec.FeedNALU(nalu)
		if ferr != nil {
			err = ferr
			break
		}
		if f != nil {
			frames++
		}
	}
	if err == nil {
		err = dec.Flush()
	}
	if err == nil {
		t.Fatal("corrupt slice header: expected an error, got nil")
	}
	if frames != 0 {
		t.Fatalf("corrupt slice header completed %d frames, want 0", frames)
	}

	// The same decoder then decodes a complete picture.
	recFrame := feedCompletePicture(t, dec, ps, slices)
	want, werr := New().DecodeNALUs(concatNALUs(ps, slices))
	if werr != nil {
		t.Fatal(werr)
	}
	if !bytes.Equal(recFrame.YUV420Bytes(), want.YUV420Bytes()) {
		t.Error("frame after a corrupt-header failure differs")
	}
}

// After a failed picture — truncated slice data, a corrupt header, or an
// incomplete picture at Flush — the same decoder instance decodes the next
// picture's slices normally, with no coverage state carried over from the
// failed picture.
func TestDecoderRecoversAfterFailedPicture(t *testing.T) {
	ps, slices := slicesStream(t, "slices_4")
	want, err := New().DecodeNALUs(concatNALUs(ps, slices))
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := want.YUV420Bytes()

	t.Run("truncated slice then complete picture", func(t *testing.T) {
		dec := New()
		// Feed parameter sets, then good slices up to the last, then a cut
		// version of it: the picture must fail.
		for _, nalu := range ps {
			if _, err := dec.FeedNALU(nalu); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < len(slices)-1; i++ {
			if _, err := dec.FeedNALU(slices[i]); err != nil {
				t.Fatal(err)
			}
		}
		cut := slices[len(slices)-1]
		cut = cut[:len(cut)-4]
		if _, err := dec.FeedNALU(cut); err == nil {
			t.Fatal("truncated final slice: expected an error")
		}
		if dec.pic != nil {
			t.Fatal("failed picture was not discarded")
		}
		// Next picture on the same instance.
		if rec := feedCompletePicture(t, dec, ps, slices); !bytes.Equal(rec.YUV420Bytes(), wantBytes) {
			t.Error("recovered frame differs")
		}
	})

	t.Run("incomplete flush then complete picture", func(t *testing.T) {
		dec := New()
		for _, nalu := range concatNALUs(ps, slices[:2]) {
			if _, err := dec.FeedNALU(nalu); err != nil {
				t.Fatal(err)
			}
		}
		if err := dec.Flush(); err == nil {
			t.Fatal("Flush with an incomplete picture: expected an error")
		}
		if dec.pic != nil {
			t.Fatal("Flush did not discard the incomplete picture")
		}
		if err := dec.Flush(); err != nil {
			t.Fatalf("second Flush after discarding: %v", err)
		}
		if rec := feedCompletePicture(t, dec, ps, slices); !bytes.Equal(rec.YUV420Bytes(), wantBytes) {
			t.Error("recovered frame differs")
		}
	})

	t.Run("failed picture then two consecutive pictures", func(t *testing.T) {
		dec := New()
		for _, nalu := range ps {
			if _, err := dec.FeedNALU(nalu); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := dec.FeedNALU(slices[0][:len(slices[0])-3]); err == nil {
			t.Fatal("truncated first slice: expected an error")
		}
		// Two full pictures in sequence on the same instance.
		for range 2 {
			if rec := feedCompletePicture(t, dec, nil, slices); !bytes.Equal(rec.YUV420Bytes(), wantBytes) {
				t.Error("recovered frame differs")
			}
		}
		if err := dec.Flush(); err != nil {
			t.Fatalf("Flush after recovery: %v", err)
		}
	})
}

// A slice whose picture-identifying header fields differ from the current
// picture's starts a new picture; the unfinished one is rejected rather than
// silently merged, no frame is produced and the failed state is discarded.
func TestDifferentPictureIdentityRejectsIncomplete(t *testing.T) {
	ps, slices := slicesStream(t, "slices_4")
	dec := New()
	for _, nalu := range ps {
		if _, err := dec.FeedNALU(nalu); err != nil {
			t.Fatal(err)
		}
	}
	// First two slices cover 160 of 300 macroblocks.
	for i := 0; i < 2; i++ {
		if _, err := dec.FeedNALU(slices[i]); err != nil {
			t.Fatal(err)
		}
	}
	// Find a bit flip that keeps the header parseable but changes frame_num,
	// so the slice identifies a different picture.
	origHeader, err := avc.ParseSliceHeader(slices[2], dec.spsMap, dec.ppsMap)
	if err != nil {
		t.Fatal(err)
	}
	var mutated []byte
	for bit := 8; bit < 40; bit++ {
		candidate := flipNALBit(slices[2], bit)
		sh, perr := avc.ParseSliceHeader(candidate, dec.spsMap, dec.ppsMap)
		if perr == nil && sh.FrameNum != origHeader.FrameNum {
			mutated = candidate
			break
		}
	}
	if mutated == nil {
		t.Fatal("test setup: found no bit flip that changes frame_num while keeping the header parseable")
	}
	// The boundary must reject the slice: either as an incomplete old picture
	// or, if the new slice cannot decode on its own, with a decode error.
	// Either way the call errors, no frame is produced and the old picture is
	// discarded.
	if _, err := dec.FeedNALU(mutated); err == nil {
		t.Fatal("a slice of a different picture while the current one is incomplete: expected an error")
	}
	if dec.pic != nil {
		t.Fatal("the incomplete picture was not discarded")
	}

	// Directly check the boundary rule itself: same identifying fields keep a
	// macroblock-0 slice in the current picture, while differing fields always
	// start a new one.
	dec2 := New()
	for _, nalu := range ps {
		if _, err := dec2.FeedNALU(nalu); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := dec2.FeedNALU(slices[i]); err != nil {
			t.Fatal(err)
		}
	}
	sameHeader, err := avc.ParseSliceHeader(slices[0], dec2.spsMap, dec2.ppsMap)
	if err != nil {
		t.Fatal(err)
	}
	if dec2.pic.beginsNextPicture(sameHeader) {
		t.Error("a macroblock-0 slice with identical identifying fields must stay in the current picture")
	}
	diffHeader, err := avc.ParseSliceHeader(mutated, dec2.spsMap, dec2.ppsMap)
	if err != nil {
		t.Fatal(err)
	}
	if !dec2.pic.beginsNextPicture(diffHeader) {
		t.Error("a slice with differing identifying fields must start a new picture")
	}
}

// Batch decoding across a failed picture followed by a good one returns the
// error for the bad picture and leaves the decoder able to decode the good
// picture on a subsequent call.
func TestBatchErrorLeavesDecoderReusable(t *testing.T) {
	ps, slices := slicesStream(t, "cavlc_slices_4")
	want, err := New().DecodeNALUs(concatNALUs(ps, slices))
	if err != nil {
		t.Fatal(err)
	}

	dec := New()
	bad := concatNALUs(ps, slices[:len(slices)-1]) // missing final slice
	if _, err := dec.DecodeIDRFrames(bad); err == nil {
		t.Fatal("incomplete batch: expected an error")
	}
	good, err := dec.DecodeIDRFrames(concatNALUs(ps, slices))
	if err != nil {
		t.Fatalf("decoder not reusable after a failed batch: %v", err)
	}
	if len(good) != 1 || !bytes.Equal(good[0].YUV420Bytes(), want.YUV420Bytes()) {
		t.Error("frame after a failed batch differs")
	}
}

// feedCompletePicture feeds the slices of one picture (parameter sets first
// when ps is non-nil) and returns the completed frame.
func feedCompletePicture(t *testing.T, dec *Decoder, ps, slices [][]byte) *frame.Frame {
	t.Helper()
	for _, nalu := range ps {
		if _, err := dec.FeedNALU(nalu); err != nil {
			t.Fatal(err)
		}
	}
	for _, nalu := range slices {
		f, err := dec.FeedNALU(nalu)
		if err != nil {
			t.Fatalf("recovery slice: %v", err)
		}
		if f != nil {
			return f
		}
	}
	t.Fatal("recovery picture never completed")
	return nil
}

// replaceSlice returns a copy of slices where index i is replaced by repl.
func replaceSlice(slices [][]byte, i int, repl []byte) [][]byte {
	out := make([][]byte, len(slices))
	copy(out, slices)
	out[i] = repl
	return out
}

// sliceHeaderSize parses the slice header of nalu and returns its size in
// bytes, so truncation tests cut inside the slice data rather than the header.
func sliceHeaderSize(t *testing.T, ps [][]byte, nalu []byte) uint32 {
	t.Helper()
	dec := New()
	for _, p := range ps {
		if _, err := dec.FeedNALU(p); err != nil {
			t.Fatal(err)
		}
	}
	sh, err := avc.ParseSliceHeader(nalu, dec.spsMap, dec.ppsMap)
	if err != nil {
		t.Fatalf("parse slice header: %v", err)
	}
	return sh.Size
}

// flipNALBit flips bit (counted from the MSB of byte 0) in a NAL unit.
func flipNALBit(nalu []byte, bit int) []byte {
	out := append([]byte(nil), nalu...)
	out[bit/8] ^= 1 << (7 - bit%8)
	return out
}

// A guard against accidental drift: the four-slice golden streams each carry
// 300 macroblocks and begin slices at macroblocks 0, 80, 160 and 220.
func TestMultiSliceLayoutGuard(t *testing.T) {
	ps, slices := slicesStream(t, "slices_4")
	dec := New()
	for _, p := range ps {
		if _, err := dec.FeedNALU(p); err != nil {
			t.Fatal(err)
		}
	}
	var firsts []uint32
	for _, sl := range slices {
		sh, err := avc.ParseSliceHeader(sl, dec.spsMap, dec.ppsMap)
		if err != nil {
			t.Fatal(err)
		}
		firsts = append(firsts, sh.FirstMBInSlice)
	}
	if got := fmt.Sprint(firsts); got != "[0 80 160 220]" {
		t.Fatalf("slice first macroblocks = %s, want [0 80 160 220]", got)
	}
}

// FeedNALU with DecodePSkip decodes an IDR followed by a P_Skip slice in
// streaming mode, returning one frame each, matching the batch behavior.
func TestFeedNALUPSkip(t *testing.T) {
	sps, pps, idr, pSkip := cabacStream(t, "xyxy,yxyx")
	dec := New()
	dec.DecodePSkip = true

	var idrFrame, pFrame *frame.Frame
	for _, nalu := range [][]byte{sps, pps, idr} {
		f, err := dec.FeedNALU(nalu)
		if err != nil {
			t.Fatalf("IDR phase: %v", err)
		}
		if f != nil {
			idrFrame = f
		}
	}
	if idrFrame == nil {
		t.Fatal("IDR picture did not complete")
	}
	pf, err := dec.FeedNALU(pSkip)
	if err != nil {
		t.Fatalf("P_Skip: %v", err)
	}
	pFrame = pf
	if pFrame == nil {
		t.Fatal("P_Skip produced no frame")
	}
	if !bytes.Equal(pFrame.YUV420Bytes(), idrFrame.YUV420Bytes()) {
		t.Error("P_Skip frame differs from the IDR reference it copies")
	}
	if err := dec.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// After a failed multi-slice picture, the same streaming decoder decodes a
// fresh single-slice IDR and then a P_Skip slice, with no state carried over.
func TestFeedNALURecoversToIDRAndPSkip(t *testing.T) {
	goldenPS, goldenSlices := slicesStream(t, "slices_4")
	sps, pps, idr, pSkip := cabacStream(t, "xyxy,yxyx")

	dec := New()
	dec.DecodePSkip = true
	for _, nalu := range goldenPS {
		if _, err := dec.FeedNALU(nalu); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := dec.FeedNALU(goldenSlices[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dec.FeedNALU(goldenSlices[3][:len(goldenSlices[3])-4]); err == nil {
		t.Fatal("truncated final slice: expected an error")
	}
	if dec.pic != nil {
		t.Fatal("failed picture was not discarded")
	}

	// A complete single-slice IDR, then P_Skip, on the same instance.
	for _, nalu := range [][]byte{sps, pps} {
		if _, err := dec.FeedNALU(nalu); err != nil {
			t.Fatal(err)
		}
	}
	idrFrame, err := dec.FeedNALU(idr)
	if err != nil {
		t.Fatalf("recovery IDR: %v", err)
	}
	if idrFrame == nil {
		t.Fatal("recovery IDR produced no frame")
	}
	pf, err := dec.FeedNALU(pSkip)
	if err != nil {
		t.Fatalf("recovery P_Skip: %v", err)
	}
	if pf == nil || !bytes.Equal(pf.YUV420Bytes(), idrFrame.YUV420Bytes()) {
		t.Error("P_Skip after recovery does not copy the recovered reference")
	}
	if err := dec.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

// Several multi-slice pictures fed one NALU at a time on a single decoder —
// including an out-of-order picture followed by an in-order one — each
// reconstruct to the same frame as an in-order single-picture decode.
func TestFeedNALUMultiplePictures(t *testing.T) {
	for _, name := range []string{"slices_4", "cavlc_slices_4"} {
		t.Run(name, func(t *testing.T) {
			ps, slices := slicesStream(t, name)
			want, err := New().DecodeNALUs(concatNALUs(ps, slices))
			if err != nil {
				t.Fatal(err)
			}
			wantBytes := want.YUV420Bytes()

			dec := New()
			for _, nalu := range ps {
				if _, err := dec.FeedNALU(nalu); err != nil {
					t.Fatal(err)
				}
			}

			// First picture delivered out of order.
			var count int
			for _, i := range []int{3, 1, 0, 2} {
				f, ferr := dec.FeedNALU(slices[i])
				if ferr != nil {
					t.Fatalf("shuffled picture: %v", ferr)
				}
				if f != nil {
					count++
					if !bytes.Equal(f.YUV420Bytes(), wantBytes) {
						t.Error("shuffled picture differs")
					}
				}
			}
			// Second picture delivered in order, twice.
			for range 2 {
				var got []byte
				for _, nalu := range slices {
					f, ferr := dec.FeedNALU(nalu)
					if ferr != nil {
						t.Fatalf("in-order picture: %v", ferr)
					}
					if f != nil {
						got = f.YUV420Bytes()
					}
				}
				if !bytes.Equal(got, wantBytes) {
					t.Error("in-order picture differs")
				}
				count++
			}
			if count != 3 {
				t.Fatalf("completed %d pictures, want 3", count)
			}
			if err := dec.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
		})
	}
}
