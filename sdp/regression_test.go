package sdp

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// roundTrip parses data and re-encodes it, reporting the produced text.
func roundTrip(t *testing.T, data string) string {
	t.Helper()
	sess, err := ParseString(data)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	return sess.String()
}

// A canonical description must survive a parse/encode round-trip unchanged.
func TestRoundTripExact(t *testing.T) {
	cases := map[string]string{
		// Static payload only listed on the m= line: no synthetic rtpmap.
		"static payload": "v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"t=0 0\r\n" +
			"m=audio 49170 RTP/AVP 0\r\n",

		// Explicit rtpmap on a well-known static payload must be retained.
		"static payload with explicit rtpmap": "v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"t=0 0\r\n" +
			"m=audio 49170 RTP/AVP 0 8\r\n" +
			"a=rtpmap:0 PCMU/8000\r\n" +
			"a=rtpmap:8 PCMA/8000\r\n",

		// Payload 35 is the first value past the well-known table (length 35).
		"dynamic payload 35": "v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"t=0 0\r\n" +
			"m=video 50000 RTP/AVP 35\r\n" +
			"a=rtpmap:35 H263-1998/90000\r\n",

		// rtpmap/fmtp/rtcp-fb in arbitrary order, fmtp and fb first. The
		// encoder canonicalizes attribute order; semantic content and a
		// stable second round-trip are asserted separately below.
		"attributes out of order": "v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"t=0 0\r\n" +
			"m=video 50000 RTP/AVP 100\r\n" +
			"a=fmtp:100 profile-level-id=42e01f\r\n" +
			"a=rtcp-fb:100 nack\r\n" +
			"a=rtcp-fb:100 ccm fir\r\n" +
			"a=rtpmap:100 H264/90000\r\n",

		// m= port count, connection TTL and an explicit address count of 1.
		"port count ttl and address count": "v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"c=IN IP4 224.2.17.12/127/3\r\n" +
			"t=0 0\r\n" +
			"m=video 11824/2 RTP/AVP 125\r\n" +
			"c=IN IP4 224.2.17.13/64/1\r\n" +
			"a=rtpmap:125 H264/90000\r\n",

		// IPv6 connection with an address count.
		"ipv6 address count": "v=0\r\n" +
			"o=- 0 0 IN IP6 ::1\r\n" +
			"s=-\r\n" +
			"c=IN IP6 2001:db8::1/3\r\n" +
			"t=0 0\r\n" +
			"m=video 11824/2 RTP/AVP 125\r\n" +
			"a=rtpmap:125 H264/90000\r\n",
	}
	// Canonical output for the out-of-order input: same semantics, attributes
	// emitted in the encoder's fixed rtpmap/rtcp-fb/fmtp order.
	canonical := map[string]string{
		"attributes out of order": "v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"t=0 0\r\n" +
			"m=video 50000 RTP/AVP 100\r\n" +
			"a=rtpmap:100 H264/90000\r\n" +
			"a=rtcp-fb:100 nack\r\n" +
			"a=rtcp-fb:100 ccm fir\r\n" +
			"a=fmtp:100 profile-level-id=42e01f\r\n",
	}
	for name, data := range cases {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			want := data
			if c, ok := canonical[name]; ok {
				want = c
			}
			if got := roundTrip(t, data); got != want {
				t.Fatalf("round-trip changed the description:\nwant %q\n got %q", want, got)
			}
			// A second round-trip must be stable as well.
			if got := roundTrip(t, roundTrip(t, data)); got != want {
				t.Fatalf("second round-trip changed the description: %q", got)
			}
		})
	}
}

// Static payload types listed on the m= line keep their well-known identity
// without producing an rtpmap attribute.
func TestStaticPayloadIdentity(t *testing.T) {
	sess, err := ParseString("v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 49170 RTP/AVP 0 8 9\r\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []Format{
		{Payload: 0, Name: "PCMU", ClockRate: 8000, Channels: 1},
		{Payload: 8, Name: "PCMA", ClockRate: 8000, Channels: 1},
		{Payload: 9, Name: "G722", ClockRate: 8000, Channels: 1},
	}
	for i, f := range sess.Media[0].Format {
		got := Format{
			Payload: f.Payload, Name: f.Name, ClockRate: f.ClockRate, Channels: f.Channels,
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Errorf("format %d: got %+v, want %+v", i, got, want[i])
		}
	}
	out := sess.String()
	if strings.Contains(out, "rtpmap") {
		t.Fatalf("static payloads gained a synthetic rtpmap:\n%s", out)
	}
}

// rtpmap, fmtp and rtcp-fb appearing in any order merge into one Format and
// all parameters and feedback values survive.
func TestFormatMergingAnyOrder(t *testing.T) {
	orders := []string{
		// fmtp, rtcp-fb before rtpmap (format created implicitly).
		"a=fmtp:100 profile-level-id=42e01f\r\n" +
			"a=rtcp-fb:100 nack\r\n" +
			"a=rtcp-fb:100 ccm fir\r\n" +
			"a=rtpmap:100 H264/90000\r\n",
		// rtpmap first, then the rest.
		"a=rtpmap:100 H264/90000\r\n" +
			"a=rtcp-fb:100 nack\r\n" +
			"a=fmtp:100 profile-level-id=42e01f\r\n" +
			"a=rtcp-fb:100 ccm fir\r\n",
	}
	for i, attrs := range orders {
		data := "v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"t=0 0\r\n" +
			"m=video 50000 RTP/AVP 100\r\n" + attrs
		sess, err := ParseString(data)
		if err != nil {
			t.Fatalf("order %d: %v", i, err)
		}
		formats := sess.Media[0].Format
		if len(formats) != 1 {
			t.Fatalf("order %d: expected a single merged format, got %d", i, len(formats))
		}
		f := formats[0]
		if f.Name != "H264" || f.ClockRate != 90000 {
			t.Errorf("order %d: rtpmap lost: %+v", i, f)
		}
		if !reflect.DeepEqual(f.Params, []string{"profile-level-id=42e01f"}) {
			t.Errorf("order %d: fmtp lost: %v", i, f.Params)
		}
		if !reflect.DeepEqual(f.Feedback, []string{"nack", "ccm fir"}) {
			t.Errorf("order %d: rtcp-fb lost: %v", i, f.Feedback)
		}
	}
}

// A wildcard rtcp-fb applies to every format and must not be dropped when it
// arrives before the rtpmap lines.
func TestWildcardFeedback(t *testing.T) {
	data := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=video 50000 RTP/AVP 96 97\r\n" +
		"a=rtcp-fb:* nack\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=rtpmap:97 VP8/90000\r\n"
	sess, err := ParseString(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range sess.Media[0].Format {
		if !reflect.DeepEqual(f.Feedback, []string{"nack"}) {
			t.Errorf("payload %d missing wildcard feedback: %v", f.Payload, f.Feedback)
		}
	}
}

// Encoder reuse: encoding a second session must leave no residue of the first.
func TestEncoderReuseNoResidue(t *testing.T) {
	first, err := ParseString("v=0\r\n" +
		"o=- 1 1 IN IP4 127.0.0.1\r\n" +
		"s=first-session\r\n" +
		"t=0 0\r\n" +
		"m=audio 10000 RTP/AVP 0\r\n")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseString("v=0\r\n" +
		"o=- 2 2 IN IP4 127.0.0.1\r\n" +
		"s=second-session\r\n" +
		"t=0 0\r\n" +
		"m=video 20000 RTP/AVP 99\r\n" +
		"a=rtpmap:99 H264/90000\r\n")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEncoder(nil)
	if err := e.Encode(first); err != nil {
		t.Fatal(err)
	}
	if err := e.Encode(second); err != nil {
		t.Fatal(err)
	}
	out := e.String()
	if strings.Contains(out, "first-session") || strings.Contains(out, "audio 10000") {
		t.Fatalf("previous session residue present:\n%s", out)
	}
	if want := second.String(); out != want {
		t.Fatalf("second encoding mismatch:\nwant %q\n got %q", want, out)
	}
}

type failOnceWriter struct {
	failed bool
	buf    bytes.Buffer
}

func (w *failOnceWriter) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, errors.New("simulated write failure")
	}
	return w.buf.Write(p)
}

// After a failed write, encoding again must produce the complete result.
func TestEncoderRetryAfterWriteFailure(t *testing.T) {
	sess, err := ParseString("v=0\r\n" +
		"o=- 2 2 IN IP4 127.0.0.1\r\n" +
		"s=retry-session\r\n" +
		"t=0 0\r\n" +
		"m=video 20000 RTP/AVP 99\r\n" +
		"a=rtpmap:99 H264/90000\r\n")
	if err != nil {
		t.Fatal(err)
	}
	w := &failOnceWriter{}
	e := NewEncoder(w)
	if err := e.Encode(sess); err == nil {
		t.Fatal("expected a write error")
	}
	if err := e.Encode(sess); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	want := sess.String()
	if got := w.buf.String(); got != want {
		t.Fatalf("retry produced incomplete output:\nwant %q\n got %q", want, got)
	}
}

// Looking up a payload type that is not present returns nil and leaves the
// media section untouched.
func TestFormatByPayloadMissing(t *testing.T) {
	data := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 49170 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n"
	sess, err := ParseString(data)
	if err != nil {
		t.Fatal(err)
	}
	m := sess.Media[0]
	if f := m.FormatByPayload(42); f != nil {
		t.Fatalf("expected nil for unknown payload, got %+v", f)
	}
	if len(m.Format) != 1 || m.Format[0].Payload != 0 || m.Format[0].Name != "PCMU" {
		t.Fatalf("media section modified by missing lookup: %+v", m.Format)
	}
	if f := m.FormatByPayload(0); f == nil || f.Name != "PCMU" {
		t.Fatalf("existing payload lookup failed: %+v", f)
	}
}

// String parsing and streaming decoding of the same input yield equal
// structures.
func TestStringAndStreamEquivalence(t *testing.T) {
	data := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"c=IN IP4 224.2.17.12/127/3\r\n" +
		"t=0 0\r\n" +
		"m=video 11824/2 RTP/AVP 0 100\r\n" +
		"a=fmtp:100 x=1\r\n" +
		"a=rtcp-fb:100 nack\r\n" +
		"a=rtpmap:100 H264/90000\r\n"
	fromString, err := ParseString(data)
	if err != nil {
		t.Fatal(err)
	}
	fromStream, err := NewDecoder(strings.NewReader(data)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromString, fromStream) {
		t.Fatalf("string and stream decoding differ:\n%#v\n%#v", fromString, fromStream)
	}
	// Also verify the io.Reader path through Parse via a pipe-like reader.
	if _, err := NewDecoder(io.MultiReader(strings.NewReader(data))).Decode(); err != nil {
		t.Fatal(err)
	}
}
