// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Payloader payloads a byte array for use as rtp.Packet payloads.
type Payloader interface {
	Payload(mtu uint16, payload []byte) [][]byte
}

// ErrAbsSendTimeExtension is returned when the AbsSendTime header extension
// could not be attached to the last packet of a frame. When it is returned,
// Packetize still returns the complete set of packets for the frame, just
// without the extension, and the consumed sequence numbers and advanced
// timestamp are identical to the success path.
var ErrAbsSendTimeExtension = errors.New("failed to write AbsSendTime extension")

// Packetizer packetizes a payload.
type Packetizer interface {
	// Packetize splits a frame into one or more RTP packets.
	//
	// All packets of a frame share the same timestamp and use consecutive
	// sequence numbers. When the AbsSendTime extension is enabled, it is
	// attached to the last packet of the frame only.
	//
	// An empty payload does not produce any packet (and therefore does not
	// consume sequence numbers), it only advances the timestamp by samples.
	//
	// If attaching the AbsSendTime extension fails, the full set of packets
	// is still returned (without the extension) together with a non-nil
	// error wrapping ErrAbsSendTimeExtension. The sequence numbers consumed
	// and the timestamp advanced are the same as on the success path, so a
	// later frame continues with consecutive sequence numbers and a
	// monotonically increasing timestamp.
	Packetize(payload []byte, samples uint32) ([]*Packet, error)
	GeneratePadding(samples uint32) []*Packet
	EnableAbsSendTime(value int)
	SkipSamples(skippedSamples uint32)
}

type packetizer struct {
	// mu guards every field below. A single packetizer may be shared by
	// multiple sending goroutines.
	mu sync.Mutex

	MTU         uint16
	PayloadType uint8
	SSRC        uint32
	Payloader   Payloader
	Sequencer   Sequencer
	Timestamp   uint32

	// Deprecated: will be removed in a future version.
	ClockRate uint32

	// put extension numbers in here. If they're 0, the extension is disabled (0 is not a legal extension number)
	extensionNumbers struct {
		AbsSendTime int // http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time
	}
	timegen func() time.Time
}

// NewPacketizer returns a new instance of a Packetizer for a specific payloader.
func NewPacketizer(
	mtu uint16,
	pt uint8,
	ssrc uint32,
	payloader Payloader,
	sequencer Sequencer,
	clockRate uint32,
) Packetizer {
	return &packetizer{
		MTU:         mtu,
		PayloadType: pt,
		SSRC:        ssrc,
		Payloader:   payloader,
		Sequencer:   sequencer,
		Timestamp:   globalMathRandomGenerator.Uint32(),
		ClockRate:   clockRate,
		timegen:     time.Now,
	}
}

// WithSSRC sets the SSRC for the Packetizer.
func WithSSRC(ssrc uint32) func(*packetizer) {
	return func(p *packetizer) {
		p.SSRC = ssrc
	}
}

// WithPayloadType sets the PayloadType for the Packetizer.
func WithPayloadType(pt uint8) func(*packetizer) {
	return func(p *packetizer) {
		p.PayloadType = pt
	}
}

// WithTimestamp sets the initial Timestamp for the Packetizer.
func WithTimestamp(timestamp uint32) func(*packetizer) {
	return func(p *packetizer) {
		p.Timestamp = timestamp
	}
}

// PacketizerOption is a function that configures a RTP Packetizer.
type PacketizerOption func(*packetizer)

// NewPacketizerWithOptions returns a new instance of a Packetizer with the given options.
func NewPacketizerWithOptions(
	mtu uint16,
	payloader Payloader,
	sequencer Sequencer,
	clockRate uint32,
	options ...PacketizerOption,
) Packetizer {
	packetizerInstance := &packetizer{
		MTU:       mtu,
		Payloader: payloader,
		Sequencer: sequencer,
		Timestamp: globalMathRandomGenerator.Uint32(),
		ClockRate: clockRate,
		timegen:   time.Now,
	}

	for _, option := range options {
		option(packetizerInstance)
	}

	return packetizerInstance
}

func (p *packetizer) EnableAbsSendTime(value int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.extensionNumbers.AbsSendTime = value
}

// Packetize packetizes the payload of an RTP packet and returns one or more RTP packets.
func (p *packetizer) Packetize(payload []byte, samples uint32) ([]*Packet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Guard against an empty payload: only advance the timestamp, do not
	// consume any sequence number. Returning nil without an error keeps this
	// distinguishable from an extension-write failure, which always returns
	// the packets together with a non-nil error.
	if len(payload) == 0 {
		p.Timestamp += samples

		return nil, nil //nolint:nilnil // documented empty-payload contract
	}

	payloads := p.Payloader.Payload(p.MTU-12, payload)
	packets := make([]*Packet, len(payloads))

	for i, pp := range payloads {
		packets[i] = &Packet{
			Header: Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				Marker:         i == len(payloads)-1,
				PayloadType:    p.PayloadType,
				SequenceNumber: p.Sequencer.NextSequenceNumber(),
				Timestamp:      p.Timestamp, // Figure out how to do timestamps
				SSRC:           p.SSRC,
			},
			Payload: pp,
		}
	}
	// The timestamp is advanced before the extension is written, so a failed
	// extension write cannot roll it back or make the next frame's timestamp
	// go backwards.
	p.Timestamp += samples

	// Extension id 0 means the extension is disabled: write nothing and do
	// not change the number of produced packets.
	if len(packets) != 0 && p.extensionNumbers.AbsSendTime != 0 {
		if err := p.writeAbsSendTime(packets[len(packets)-1]); err != nil {
			// Do not drop the frame: return the packets without the
			// extension together with a diagnosable error. The sequence
			// numbers above and the timestamp advance above are already
			// committed, matching the success path exactly.
			return packets, fmt.Errorf("%w: %w", ErrAbsSendTimeExtension, err)
		}
	}

	return packets, nil
}

// writeAbsSendTime attaches the AbsSendTime extension (see
// http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time) to the last
// packet of a frame. The packet is left untouched when an error is returned.
func (p *packetizer) writeAbsSendTime(packet *Packet) error {
	id := uint8(p.extensionNumbers.AbsSendTime) // nolint: gosec // G115
	// Packets produced by Packetize always start without extensions and the
	// 3-byte AbsSendTime payload therefore uses the one-byte header extension
	// profile, whose legal ids are 1..14 and payloads must be at most 16
	// bytes. Validate explicitly instead of silently emitting a packet with
	// the reserved id or an illegal extension size.
	if err := headerExtensionCheck(ExtensionProfileOneByte, id, make([]byte, absSendTimeExtensionSize)); err != nil {
		return fmt.Errorf("invalid AbsSendTime extension id %d: %w", id, err)
	}

	sendTime := NewAbsSendTimeExtension(p.timegen())
	b, err := sendTime.Marshal()
	if err != nil {
		return fmt.Errorf("marshal AbsSendTime: %w", err)
	}
	if err := packet.SetExtension(id, b); err != nil {
		return fmt.Errorf("set AbsSendTime extension: %w", err)
	}

	return nil
}

// GeneratePadding returns required padding-only packages.
func (p *packetizer) GeneratePadding(samples uint32) []*Packet {
	// Guard against an empty payload
	if samples == 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	packets := make([]*Packet, samples)

	for i := 0; i < int(samples); i++ {
		packets[i] = &Packet{
			Header: Header{
				Version:        2,
				Padding:        true,
				Extension:      false,
				Marker:         false,
				PayloadType:    p.PayloadType,
				SequenceNumber: p.Sequencer.NextSequenceNumber(),
				Timestamp:      p.Timestamp, // Use latest timestamp
				SSRC:           p.SSRC,
				PaddingSize:    255,
			},
		}
	}

	return packets
}

// SkipSamples causes a gap in sample count between Packetize requests so the
// RTP payloads produced have a gap in timestamps.
func (p *packetizer) SkipSamples(skippedSamples uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Timestamp += skippedSamples
}
