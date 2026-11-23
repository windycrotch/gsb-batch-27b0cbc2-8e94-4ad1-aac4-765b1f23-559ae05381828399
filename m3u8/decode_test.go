package m3u8

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const validMediaPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:40
#EXTINF:10,
seg-40.ts
#EXTINF:9.5,
seg-41.ts
#EXTINF:8.25,third segment
seg-42.ts
#EXT-X-ENDLIST
`

// decodeOrTimeout runs Decode and fails the test if it does not return,
// guarding against regressions where the parser blocks on bad input.
func decodeOrTimeout(t *testing.T, in string) (*Playlist, error) {
	t.Helper()
	type result struct {
		p   *Playlist
		err error
	}
	ch := make(chan result, 1)
	go func() {
		p, err := Decode(strings.NewReader(in))
		ch <- result{p, err}
	}()
	select {
	case r := <-ch:
		return r.p, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("Decode did not return: parser is stuck")
		return nil, nil
	}
}

func TestDecodeValidPlaylist(t *testing.T) {
	p, err := Decode(strings.NewReader(validMediaPlaylist))
	if err != nil {
		t.Fatalf("decode valid playlist: %v", err)
	}
	if p.Version != 3 {
		t.Errorf("version = %d, want 3", p.Version)
	}
	if p.TargetDuration != 10*time.Second {
		t.Errorf("target duration = %s, want 10s", p.TargetDuration)
	}
	if p.Sequence != 40 {
		t.Errorf("sequence = %d, want 40", p.Sequence)
	}
	if !p.End {
		t.Error("end of playlist not set")
	}
	want := []Segment{
		{URI: "seg-40.ts", Duration: 10 * time.Second},
		{URI: "seg-41.ts", Duration: 9500 * time.Millisecond},
		{URI: "seg-42.ts", Duration: 8250 * time.Millisecond, Title: "third segment"},
	}
	if len(p.Segments) != len(want) {
		t.Fatalf("got %d segments, want %d", len(p.Segments), len(want))
	}
	for i, seg := range p.Segments {
		if seg.URI != want[i].URI {
			t.Errorf("segment %d: URI = %q, want %q", i, seg.URI, want[i].URI)
		}
		if seg.Duration != want[i].Duration {
			t.Errorf("segment %d: duration = %s, want %s", i, seg.Duration, want[i].Duration)
		}
		if seg.Title != want[i].Title {
			t.Errorf("segment %d: title = %q, want %q", i, seg.Title, want[i].Title)
		}
	}
}

// Playlists in testdata must keep decoding to the same results.
func TestDecodeGoldenMedia(t *testing.T) {
	f, err := os.Open("testdata/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Decode(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Version != 3 {
		t.Errorf("version = %d, want 3", p.Version)
	}
	if p.TargetDuration != 10*time.Second {
		t.Errorf("target duration = %s, want 10s", p.TargetDuration)
	}
	if len(p.Segments) != 51 {
		t.Fatalf("got %d segments, want 51", len(p.Segments))
	}
	for i, seg := range p.Segments {
		if seg.Duration != 10*time.Second {
			t.Errorf("segment %d: duration = %s, want 10s", i, seg.Duration)
		}
		want := fmt.Sprintf("stream_110k_48k_416x234_%03d.ts", i)
		if seg.URI != want {
			t.Errorf("segment %d: URI = %q, want %q", i, seg.URI, want)
		}
	}
	if !p.End {
		t.Error("end of playlist not set")
	}
}

func TestDecodeGoldenByteRange(t *testing.T) {
	f, err := os.Open("testdata/media_x_byterange.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Decode(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Version != 6 {
		t.Errorf("version = %d, want 6", p.Version)
	}
	if p.Type != PlaylistVOD {
		t.Errorf("playlist type = %s, want VOD", p.Type)
	}
	ranges := []ByteRange{{4587770, 14872}, {2347999, 0}, {341857, 0}, {57551, 0}}
	if len(p.Segments) != len(ranges) {
		t.Fatalf("got %d segments, want %d", len(p.Segments), len(ranges))
	}
	for i, seg := range p.Segments {
		if seg.Range != ranges[i] {
			t.Errorf("segment %d: range = %v, want %v", i, seg.Range, ranges[i])
		}
		if seg.URI != "primary.mp4" {
			t.Errorf("segment %d: URI = %q, want primary.mp4", i, seg.URI)
		}
		if seg.Duration != 6*time.Second {
			t.Errorf("segment %d: duration = %s, want 6s", i, seg.Duration)
		}
	}
	first := p.Segments[0]
	if first.Map == nil {
		t.Fatal("first segment has no map")
	}
	if first.Map.URI != "primary.mp4" {
		t.Errorf("map URI = %q, want primary.mp4", first.Map.URI)
	}
	if first.Map.ByteRange != (ByteRange{872, 0}) {
		t.Errorf("map byte range = %v, want {872 0}", first.Map.ByteRange)
	}
	for i := 1; i < len(p.Segments); i++ {
		if p.Segments[i].Map != nil {
			t.Errorf("segment %d: unexpected map carried over from first segment", i)
		}
	}
	if !p.End {
		t.Error("end of playlist not set")
	}
}

func TestDecodeGoldenMasterEndList(t *testing.T) {
	f, err := os.Open("testdata/master_end_list.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Decode(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	uris := []string{
		"0_hd_hls/0_hd_600.m3u8",
		"0_hd_hls/0_hd_1000.m3u8",
		"0_hd_hls/0_hd_2000.m3u8",
	}
	if len(p.Variants) != len(uris) {
		t.Fatalf("got %d variants, want %d", len(p.Variants), len(uris))
	}
	for i, v := range p.Variants {
		if v.URI != uris[i] {
			t.Errorf("variant %d: URI = %q, want %q", i, v.URI, uris[i])
		}
	}
	if !p.End {
		t.Error("end of playlist not set")
	}
}

func TestDecodeBadInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"missing head tag", "#EXT-X-VERSION:3\n"},
		{"truncated segment", "#EXTM3U\n#EXTINF:10,\n"},
		{"truncated key", "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,"},
		{"truncated variant", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1280000\n"},
		{"unterminated quote", "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\n#EXTINF:10,\na.ts\n"},
		{"illegal character", "#EXTM3U\n#EXT-X-TARGETDURATION:$\n"},
		{"bad duration", "#EXTM3U\n#EXTINF:abc\na.ts\n"},
		{"bad byte range", "#EXTM3U\n#EXTINF:10,\n#EXT-X-BYTERANGE:732@\na.ts\n"},
		{"bad version", "#EXTM3U\n#EXT-X-VERSION:\n"},
		{"bad encrypt method", "#EXTM3U\n#EXT-X-KEY:METHOD=ROT13\n#EXTINF:10,\na.ts\n"},
		{"unknown variant attribute", "#EXTM3U\n#EXT-X-STREAM-INF:FOO=1\nx.m3u8\n"},
		{"huge line", "#EXTM3U\n" + strings.Repeat("a", 1<<20) + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := decodeOrTimeout(t, tt.in)
			if err == nil {
				t.Errorf("Decode(%q): nil error, want failure", tt.in)
			}
			if p != nil {
				t.Errorf("Decode(%q): got partial playlist, want nil on error", tt.in)
			}
		})
	}
}

// Errors must identify the offending tag or attribute so callers can
// locate the problem in their input.
func TestDecodeErrorLocatesTag(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unknown variant attribute", "#EXTM3U\n#EXT-X-STREAM-INF:FOO=1\nx.m3u8\n", "FOO"},
		{"bad encrypt method", "#EXTM3U\n#EXT-X-KEY:METHOD=ROT13\n#EXTINF:10,\na.ts\n", "ROT13"},
		{"bad rendition attribute", "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,BOGUS=1\n", "BOGUS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeOrTimeout(t, tt.in)
			if err == nil {
				t.Fatalf("Decode(%q): nil error", tt.in)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// Lexically valid but unsupported tags must not fail the parse, and
// must not swallow items belonging to the following tags.
func TestDecodeUnsupportedTagsSkipped(t *testing.T) {
	in := `#EXTM3U
#EXT-X-TARGETDURATION:10
#EXT-X-ALLOW-CACHE:YES
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=25000,URI="iframe.m3u8"
#EXT-X-SOME-FUTURE-TAG:FOO=1,BAR="two",BAZ=3.5
#EXTINF:10,
a.ts
#EXT-X-ENDLIST
`
	p, err := decodeOrTimeout(t, in)
	if err != nil {
		t.Fatalf("decode playlist with unsupported tags: %v", err)
	}
	if len(p.Segments) != 1 {
		t.Fatalf("got %d segments, want 1", len(p.Segments))
	}
	seg := p.Segments[0]
	if seg.URI != "a.ts" || seg.Duration != 10*time.Second {
		t.Errorf("segment = %v, want {URI:a.ts Duration:10s}", seg)
	}
	if !p.End {
		t.Error("end of playlist not set")
	}
}

// Tags belonging to one segment must never be attributed to an
// adjacent segment, even when unknown tags sit between them.
func TestSegmentAttribution(t *testing.T) {
	in := `#EXTM3U
#EXT-X-TARGETDURATION:10
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="key1.bin",IV=0x00000000000000000000000000000001
#EXT-X-PROGRAM-DATE-TIME:2020-01-02T03:04:05.000Z
#EXTINF:10,
a.ts
#EXT-X-UNKNOWN-CUSTOM:FOO=1,BAR="two"
#EXT-X-KEY:METHOD=NONE
#EXTINF:5,
#EXT-X-BYTERANGE:100@200
b.ts
#EXT-X-ENDLIST
`
	p, err := decodeOrTimeout(t, in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(p.Segments))
	}

	first := p.Segments[0]
	if first.URI != "a.ts" {
		t.Errorf("first segment URI = %q, want a.ts", first.URI)
	}
	if first.Duration != 10*time.Second {
		t.Errorf("first segment duration = %s, want 10s", first.Duration)
	}
	if first.Key == nil || first.Key.Method != EncryptMethodAES128 || first.Key.URI != "key1.bin" {
		t.Errorf("first segment key = %+v, want AES-128 key1.bin", first.Key)
	}
	if first.Map == nil || first.Map.URI != "init.mp4" {
		t.Errorf("first segment map = %+v, want init.mp4", first.Map)
	}
	wantTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if !first.DateTime.Equal(wantTime) {
		t.Errorf("first segment date time = %s, want %s", first.DateTime, wantTime)
	}
	if first.Range != (ByteRange{0, 0}) {
		t.Errorf("first segment range = %v, want zero", first.Range)
	}

	second := p.Segments[1]
	if second.URI != "b.ts" {
		t.Errorf("second segment URI = %q, want b.ts", second.URI)
	}
	if second.Duration != 5*time.Second {
		t.Errorf("second segment duration = %s, want 5s", second.Duration)
	}
	if second.Key == nil || second.Key.Method != EncryptMethodNone {
		t.Errorf("second segment key = %+v, want METHOD=NONE", second.Key)
	}
	if second.Range != (ByteRange{100, 200}) {
		t.Errorf("second segment range = %v, want {100 200}", second.Range)
	}
	if second.Map != nil {
		t.Errorf("second segment map = %+v, want nil", second.Map)
	}
	if !second.DateTime.IsZero() {
		t.Errorf("second segment date time = %s, want zero", second.DateTime)
	}
}

// A failed Decode must not affect subsequent calls.
func TestDecodeRepeatedIndependent(t *testing.T) {
	truncated := "#EXTM3U\n#EXTINF:10,\n"
	for i := 0; i < 25; i++ {
		if _, err := decodeOrTimeout(t, truncated); err == nil {
			t.Fatalf("iteration %d: nil error decoding truncated playlist", i)
		}
		p, err := decodeOrTimeout(t, validMediaPlaylist)
		if err != nil {
			t.Fatalf("iteration %d: decode valid playlist after failure: %v", i, err)
		}
		if len(p.Segments) != 3 {
			t.Fatalf("iteration %d: got %d segments, want 3", i, len(p.Segments))
		}
		if p.Segments[2].URI != "seg-42.ts" || p.Segments[2].Title != "third segment" {
			t.Errorf("iteration %d: last segment = %+v", i, p.Segments[2])
		}
	}
}

// Decode must not leak goroutines, even when parsing fails.
func TestDecodeNoGoroutineLeak(t *testing.T) {
	inputs := []string{
		validMediaPlaylist,
		"#EXTM3U\n#EXTINF:10,\n", // truncated segment
		"#EXTM3U\n#EXT-X-KEY:METHOD=ROT13\n#EXTINF:10,\na.ts\n",   // error inside segment
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1280000\n",          // truncated variant
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"unterminated\n", // lexer error
	}
	// warm up, then record the baseline
	for _, in := range inputs {
		Decode(strings.NewReader(in))
	}
	base := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		for _, in := range inputs {
			Decode(strings.NewReader(in))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Errorf("Decode leaked goroutines: baseline %d, after repeated calls %d", base, n)
	}
}

// Concurrent decodes of valid and invalid playlists must not interfere
// with each other.
func TestDecodeConcurrent(t *testing.T) {
	const workers = 32
	const iterations = 20
	errch := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if (i+j)%2 == 0 {
					p, err := Decode(strings.NewReader(validMediaPlaylist))
					if err != nil {
						errch <- fmt.Errorf("decode valid playlist: %w", err)
						return
					}
					if len(p.Segments) != 3 || p.Sequence != 40 {
						errch <- fmt.Errorf("valid playlist: got %d segments, sequence %d", len(p.Segments), p.Sequence)
						return
					}
				} else {
					p, err := Decode(strings.NewReader("#EXTM3U\n#EXTINF:10,\n"))
					if err == nil || p != nil {
						errch <- errors.New("truncated playlist: want error and nil playlist")
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errch)
	for err := range errch {
		t.Error(err)
	}
}
