package audioio

import (
	"context"
	"errors"
	"testing"
)

// TestDecoderCanceledContextFailsFast: a decoder bound to a canceled render
// context must report the cancellation immediately, without launching (or
// relaunching) an FFmpeg process under the dead context.
func TestDecoderCanceledContextFailsFast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &Decoder{ctx: ctx, path: "never-launched", sampleRate: 8000, channels: 1, frameBytes: nbytes}

	err := d.ReadInto(0, 16, make([]float32, 16))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadInto on canceled ctx = %v, want context.Canceled", err)
	}
	if d.proc != nil {
		t.Fatal("canceled decoder launched a process")
	}
}

// TestDecoderCloseReleasesScratch: Close returns the decode scratch buffer
// rather than holding it until some later call.
func TestDecoderCloseReleasesScratch(t *testing.T) {
	d := &Decoder{scratch: make([]byte, 128)}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d.scratch != nil {
		t.Fatal("Close kept the decoder scratch buffer")
	}
}

// TestDecoderConcurrentCloseAndRead: a render-scoped reaper may Close a
// decoder while another render sharing the node is mid-read. The read must
// either complete or see ErrClosed/context error — never race. Run with
// -race.
func TestDecoderConcurrentCloseAndRead(t *testing.T) {
	d := &Decoder{ctx: context.Background(), path: "x", sampleRate: 8000, channels: 1, frameBytes: nbytes}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Reads against a decoder with no process: eof is false, so each read
		// would try to launch — but Close lands first or after, and both
		// outcomes (ErrClosed or a launch error) are fine. We only assert the
		// absence of a data race and of a panic.
		for i := 0; i < 100; i++ {
			_ = d.ReadInto(0, 1, make([]float32, 1))
		}
	}()
	for i := 0; i < 100; i++ {
		_ = d.Close()
	}
	<-done
	if !d.Closed() {
		t.Fatal("decoder not closed after Close")
	}
}
