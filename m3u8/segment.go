package m3u8

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Media segment tags specified in RFC 8216 section 4.4.4.
const (
	tagSegmentDuration = "#EXTINF"
	tagByteRange       = "#EXT-X-BYTERANGE"
	tagDiscontinuity   = "#EXT-X-DISCONTINUITY"
	tagKey             = "#EXT-X-KEY"
	tagMap             = "#EXT-X-MAP"
	tagDateTime        = "#EXT-X-PROGRAM-DATE-TIME"
	tagGap             = "#EXT-X-GAP"
	tagBitrate         = "#EXT-X-BITRATE"
	tagPart            = "#EXT-X-PART"
	tagDateRange       = "#EXT-X-DATERANGE"
)

// parseSegment returns the next segment from p. The leading item, which
// indicated the start of the segment, has already been consumed from p.
// Items are consumed up to and including the segment's URL; items
// belonging to the next segment are never consumed, so tags such as
// EXTINF, BYTERANGE, KEY, MAP and PROGRAM-DATE-TIME are only ever
// attributed to the one segment they precede.
func parseSegment(p *parser, leading item) (*Segment, error) {
	var seg Segment
	it := leading
	for {
		switch it.typ {
		case itemError:
			return nil, errors.New(it.val)
		case itemEOF:
			return nil, fmt.Errorf("unexpected end of playlist: segment has no URL")
		case itemNewline:
			it = p.next()
			continue
		case itemURL:
			seg.URI = it.val
			return &seg, nil
		}
		if it.typ != itemTag {
			return nil, fmt.Errorf("unexpected %s %q, expected tag", it.typ, it.val)
		}

		switch it.val {
		case tagSegmentDuration:
			vit, err := p.value()
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", tagSegmentDuration, err)
			}
			if vit.typ != itemAttrName && vit.typ != itemNumber {
				return nil, fmt.Errorf("parse segment duration: unexpected %s: want attribute name or number", vit)
			}
			dur, err := parseSegmentDuration(vit.val)
			if err != nil {
				return nil, fmt.Errorf("parse segment duration: %w", err)
			}
			seg.Duration = dur

			// check for the optional segment title
			it = p.next()
			switch it.typ {
			case itemNewline:
				it = p.next()
				continue
			case itemError:
				return nil, errors.New(it.val)
			case itemEOF:
				return nil, fmt.Errorf("unexpected end of playlist after %s", tagSegmentDuration)
			}
			if it.typ != itemComma {
				return nil, fmt.Errorf("expected comma after segment duration, got %s", it)
			}
			tit, err := p.value()
			if err != nil {
				return nil, fmt.Errorf("parse segment title: %w", err)
			}
			seg.Title = tit.val

		case tagByteRange:
			it, err := p.value()
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", tagByteRange, err)
			}
			r, err := parseByteRange(it.val)
			if err != nil {
				return nil, fmt.Errorf("parse byte range: %w", err)
			}
			seg.Range = r
		case tagDiscontinuity:
			seg.Discontinuity = true
		case tagKey:
			key, err := parseKey(p)
			if err != nil {
				return nil, fmt.Errorf("parse key: %w", err)
			}
			seg.Key = &key
		case tagMap:
			m, err := parseMap(p)
			if err != nil {
				return nil, fmt.Errorf("parse map: %w", err)
			}
			seg.Map = &m
		case tagDateTime:
			it, err := p.value()
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", tagDateTime, err)
			}
			t, err := time.Parse(rfc3339Milli, it.val)
			if err != nil {
				return nil, fmt.Errorf("bad date time tag: %w", err)
			}
			seg.DateTime = t
		default:
			// Lexically valid but unsupported segment tag: ignore
			// the rest of the line rather than failing the parse.
			if err := skipLine(p); err != nil {
				return nil, fmt.Errorf("parse %s: %w", it.val, err)
			}
		}
		it = p.next()
	}
}

func parseSegmentDuration(s string) (time.Duration, error) {
	// Some numbers can be converted straight to ints, e.g.:
	// 	10
	// 	10.000
	// Others need to be converted from floating point, e.g:
	// 	9.967
	// Try the easiest paths first.
	if !strings.Contains(s, ".") {
		i, err := strconv.Atoi(s)
		if err != nil {
			return 0, err
		}
		return time.Duration(i) * time.Second, nil
	}

	// 10.000
	before, after, _ := strings.Cut(s, ".")
	var allZeroes = true
	for r := range after {
		if r != '0' {
			allZeroes = false
		}
	}
	if allZeroes {
		i, err := strconv.Atoi(before)
		if err != nil {
			return 0, err
		}
		return time.Duration(i) * time.Second, nil
	}
	seconds, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return 0, err
	}
	// precision based on a 90KHz clock.
	microseconds := seconds * 1e6
	return time.Duration(microseconds) * time.Microsecond, nil
}

func parseKey(p *parser) (Key, error) {
	var key Key
	for {
		it := p.next()
		switch it.typ {
		case itemError:
			return key, errors.New(it.val)
		case itemEOF:
			return key, fmt.Errorf("unexpected end of playlist in %s", tagKey)
		case itemNewline:
			return key, nil
		case itemComma:
			continue
		}
		if it.typ != itemAttrName {
			return Key{}, fmt.Errorf("expected attribute name, got %s", it.val)
		}
		attr := it
		v, err := p.value()
		if err != nil {
			return key, fmt.Errorf("parse %s attribute: %w", attr.val, err)
		}
		if v.typ != itemEquals {
			return key, fmt.Errorf("expected %q after %s, got %s", "=", it.typ, v)
		}

		switch attr.val {
		case "METHOD":
			v, err = p.value()
			if err != nil {
				return key, fmt.Errorf("parse METHOD attribute: %w", err)
			}
			key.Method = parseEncryptMethod(v.val)
			if key.Method == encryptMethodInvalid {
				return key, fmt.Errorf("bad encrypt method %q", v.val)
			}
		case "URI":
			v, err = p.value()
			if err != nil {
				return key, fmt.Errorf("parse URI attribute: %w", err)
			}
			key.URI = strings.Trim(v.val, `"`)
		case "IV":
			v, err = p.value()
			if err != nil {
				return key, fmt.Errorf("parse IV attribute: %w", err)
			}
			b, err := hex.DecodeString(strings.TrimPrefix(v.val, "0x"))
			if err != nil {
				return key, fmt.Errorf("parse initialisation vector: %w", err)
			}
			if len(b) != len(key.IV) {
				return key, fmt.Errorf("bad initialisation length %d, want %d", len(b), len(key.IV))
			}
			copy(key.IV[:], b)
		case "KEYFORMAT":
			v, err = p.value()
			if err != nil {
				return key, fmt.Errorf("parse KEYFORMAT attribute: %w", err)
			}
			key.Format = strings.Trim(v.val, `"`)
		case "KEYFORMATVERSIONS":
			v, err = p.value()
			if err != nil {
				return key, fmt.Errorf("parse KEYFORMATVERSIONS attribute: %w", err)
			}
			ss := strings.Split(v.val, "/")
			key.FormatVersions = make([]uint32, len(ss))
			for i := range ss {
				n, err := strconv.Atoi(ss[i])
				if err != nil {
					return key, fmt.Errorf("parse key format version: %w", err)
				}
				key.FormatVersions[i] = uint32(n)
			}
		default:
			return key, fmt.Errorf("unexpected attribute %q", it.val)
		}
	}
}

func parseMap(p *parser) (Map, error) {
	var mmap Map
	for {
		it := p.next()
		switch it.typ {
		case itemError:
			return mmap, errors.New(it.val)
		case itemEOF:
			return mmap, fmt.Errorf("unexpected end of playlist in %s", tagMap)
		case itemNewline:
			return mmap, nil
		case itemComma:
			continue
		}
		if it.typ != itemAttrName {
			return Map{}, fmt.Errorf("unexpected %s %q", it.typ, it.val)
		}
		attr := it.val
		it, err := p.value()
		if err != nil {
			return Map{}, fmt.Errorf("parse %s attribute: %w", attr, err)
		}
		if it.typ != itemEquals {
			return Map{}, fmt.Errorf("expected %q after %s, got %q", "=", attr, it.val)
		}

		it, err = p.value()
		if err != nil {
			return Map{}, fmt.Errorf("parse %s attribute: %w", attr, err)
		}
		switch attr {
		case "URI":
			mmap.URI = strings.Trim(it.val, `"`)
		case "BYTERANGE":
			r, err := parseByteRange(strings.Trim(it.val, `"`))
			if err != nil {
				return Map{}, fmt.Errorf("parse byte range: %w", err)
			}
			mmap.ByteRange = r
		default:
			return Map{}, fmt.Errorf("unexpected attribute %q", it.val)
		}
	}
}

func writeSegments(w io.Writer, segments []Segment) (n int, err error) {
	for i, seg := range segments {
		b, err := seg.MarshalText()
		if err != nil {
			return n, fmt.Errorf("segment %d: %w", i, err)
		}
		nn, err := fmt.Fprintln(w, string(b))
		n += nn
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (seg *Segment) MarshalText() ([]byte, error) {
	if seg.URI == "" {
		return nil, fmt.Errorf("empty URI")
	}
	if seg.Duration == 0 {
		return nil, fmt.Errorf("zero duration")
	}
	var tags []string
	if seg.Discontinuity {
		tags = append(tags, tagDiscontinuity)
	}
	if seg.DateRange != nil {
		buf := &bytes.Buffer{}
		if err := writeDateRange(buf, seg.DateRange); err != nil {
			return nil, fmt.Errorf("write date range: %w", err)
		}
		tags = append(tags, buf.String())
	}
	if seg.Range != [2]int{0, 0} {
		if seg.Range[0] >= seg.Range[1] {
			return nil, fmt.Errorf("impossible range: offset (%d) must be smaller than next %d", seg.Range[0], seg.Range[1])
		}
		tags = append(tags, fmt.Sprintf("%s:%s", tagByteRange, seg.Range))
	}
	if seg.Key != nil {
		tags = append(tags, seg.Key.String())
	}
	if seg.Map != nil {
		tags = append(tags, seg.Map.String())
	}
	if !seg.DateTime.IsZero() {
		tags = append(tags, fmt.Sprintf("%s:%s", tagDateTime, seg.DateTime.Format(rfc3339Milli)))
	}
	us := seg.Duration / time.Microsecond
	// we do .03f for the same precision as test-streams.mux.dev.
	durTag := fmt.Sprintf("%s:%.03f", tagSegmentDuration, float32(us)/1e6)
	if seg.Title != "" {
		durTag += "," + seg.Title
	}
	tags = append(tags, durTag)
	tags = append(tags, seg.URI)
	return []byte(strings.Join(tags, "\n")), nil
}
