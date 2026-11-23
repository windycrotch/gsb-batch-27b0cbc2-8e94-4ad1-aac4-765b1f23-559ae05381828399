package render

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestProbeRepliesPreserveKeys(t *testing.T) {
	for _, tt := range []struct {
		name, reply      string
		found, supported bool
		want             string
	}{
		{"success", "/cat\x1b_Gi=42;OK\x1b\\\r", true, true, "/cat\r"},
		{"error", "\x1b_Gi=42;EINVAL: unsupported\x1b\\q", true, false, "q"},
		{"other image", "\x1b_Gi=43;OK\x1b\\", false, false, "\x1b_Gi=43;OK\x1b\\"},
		{"other fields", "\x1b_Gp=1,i=42;OK\x1b\\", true, true, ""},
		{"incomplete", "\x1b_Gi=42;OK", false, false, "\x1b_Gi=42;OK"},
		{"keys", "\x1b[A\x03\x1b", false, false, "\x1b[A\x03\x1b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, found, supported := stripProbeReply([]byte(tt.reply), 42)
			if string(got) != tt.want || found != tt.found || supported != tt.supported {
				t.Fatalf("got %q %v %v", got, found, supported)
			}
		})
	}
}
func TestProbeCancellationAndTimeout(t *testing.T) {
	p := &ProbeInput{id: 42, result: make(chan bool, 1)}
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p.Detect(ctx, &out) {
		t.Fatal("cancelled probe succeeded")
	}
	if !strings.Contains(out.String(), "a=q") {
		t.Fatal("missing query")
	}
	start := time.Now()
	if p.Detect(context.Background(), &out) {
		t.Fatal("unanswered probe succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probe stalled for %s", elapsed)
	}
}
