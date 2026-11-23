// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestPacketizer builds a packetizer with a deterministic sequencer,
// timestamp and clock, so every expectation below is exact rather than
// statistical.
func newTestPacketizer(t *testing.T, mtu uint16) (*packetizer, Packetizer) {
	t.Helper()

	pktizer := NewPacketizerWithOptions(
		mtu,
		&codecs.G722Payloader{},
		NewFixedSequencer(1000),
		90000,
		WithPayloadType(98),
		WithSSRC(0x1234ABCD),
		WithTimestamp(50000),
	)
	p, ok := pktizer.(*packetizer)
	require.True(t, ok, "Failed to cast to *packetizer")
	p.timegen = func() time.Time {
		return time.Date(1985, time.June, 23, 4, 0, 0, 0, time.FixedZone("UTC-5", -5*60*60))
	}

	return p, pktizer
}

// lastConsumedSequence reads the sequencer state without consuming a new
// sequence number: NextSequenceNumber would mutate the counter and could
// not be used as a peek. NewFixedSequencer(1000) starts at state 999.
func lastConsumedSequence(t *testing.T, p *packetizer) uint16 {
	t.Helper()

	seq, ok := p.Sequencer.(*sequencer)
	require.True(t, ok)

	return uint16(seq.state.Load()) // nolint: gosec // G115
}

// assertFramePackets checks the per-frame invariants: every packet shares
// the frame timestamp, sequence numbers are consecutive and only the last
// packet carries the marker bit.
func assertFramePackets(t *testing.T, packets []*Packet, firstSeq uint16, timestamp uint32) {
	t.Helper()

	require.NotEmpty(t, packets)
	for i, pkt := range packets {
		assert.Equal(t, timestamp, pkt.Timestamp, "packet %d: frame timestamp must be identical", i)
		assert.Equal(t, firstSeq+uint16(i), pkt.SequenceNumber, "packet %d: sequence number must be consecutive", i) // nolint: gosec // G115
		assert.Equal(t, i == len(packets)-1, pkt.Marker, "packet %d: only the last packet may set the marker", i)
		assert.Equal(t, uint8(2), pkt.Version)
		assert.Equal(t, uint8(98), pkt.PayloadType)
		assert.Equal(t, uint32(0x1234ABCD), pkt.SSRC)
	}
}

func TestPacketize_AbsSendTime_OnlyOnLastPacket(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	pktizer.EnableAbsSendTime(1)

	// 128 bytes with an effective MTU of 88 produces two G722 payloads.
	packets, err := pktizer.Packetize(make([]byte, 128), 2000)
	require.NoError(t, err)
	require.Len(t, packets, 2)
	assertFramePackets(t, packets, 1000, 50000)

	assert.False(t, packets[0].Extension, "AbsSendTime must only be on the last packet")
	assert.Nil(t, packets[0].GetExtension(1))

	assert.True(t, packets[1].Extension, "last packet must carry the AbsSendTime extension")
	raw := packets[1].GetExtension(1)
	require.Len(t, raw, absSendTimeExtensionSize)
	assert.Equal(t, []byte{0x40, 0, 0}, raw)

	// Success path: timestamp advanced, next sequence number available.
	assert.Equal(t, uint32(50000+2000), p.Timestamp)
}

func TestPacketize_AbsSendTimeDisabled_IDZero_WritesNothing(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	// Extension id 0 means disabled: no extension may be written and the
	// packet count must be identical to an enabled-but-valid run.
	packets, err := pktizer.Packetize(make([]byte, 128), 2000)
	require.NoError(t, err)
	require.Len(t, packets, 2)
	assertFramePackets(t, packets, 1000, 50000)

	for _, pkt := range packets {
		assert.False(t, pkt.Extension, "id 0 must not enable any extension")
		assert.Empty(t, pkt.GetExtensionIDs())
	}
	assert.Equal(t, uint32(50000+2000), p.Timestamp)
	assert.Equal(t, uint16(1001), lastConsumedSequence(t, p),
		"two packets consumed seq 1000 and 1001")
}

func TestPacketize_AbsSendTimeFailure_ReturnsPacketsAndError(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	// 15 is the reserved one-byte extension id: writing the extension must
	// fail rather than silently producing an illegal packet.
	pktizer.EnableAbsSendTime(15)

	packets, err := pktizer.Packetize(make([]byte, 128), 2000)
	require.Error(t, err, "an invalid extension id must be diagnosable")
	assert.True(t, errors.Is(err, ErrAbsSendTimeExtension), "error must wrap ErrAbsSendTimeExtension: %v", err)

	// The whole frame is still delivered, just without the extension.
	require.Len(t, packets, 2, "failed extension write must not drop the frame")
	assertFramePackets(t, packets, 1000, 50000)
	for _, pkt := range packets {
		assert.False(t, pkt.Extension, "failed packet must not be left half-mutated")
		assert.Empty(t, pkt.GetExtensionIDs())
	}

	// The consumed sequence numbers and advanced timestamp match the
	// success path exactly.
	assert.Equal(t, uint32(50000+2000), p.Timestamp, "timestamp must advance even on extension failure")
	assert.Equal(t, uint16(1001), lastConsumedSequence(t, p),
		"failure must consume the same sequence numbers as a successful frame")
}

func TestPacketize_AbsSendTimeFailure_RecoveryKeepsTimestampsAndSequences(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)

	const frameSamples = uint32(2000)

	// Frame 1: success.
	pktizer.EnableAbsSendTime(1)
	ok1, err := pktizer.Packetize(make([]byte, 40), frameSamples)
	require.NoError(t, err)
	require.Len(t, ok1, 1)
	assertFramePackets(t, ok1, 1000, 50000)
	require.True(t, ok1[0].Extension)

	// Frame 2: extension write fails (reserved id 15).
	pktizer.EnableAbsSendTime(15)
	failed, err := pktizer.Packetize(make([]byte, 40), frameSamples)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAbsSendTimeExtension))
	require.Len(t, failed, 1, "failed frame must still be returned")
	assertFramePackets(t, failed, 1001, 50000+frameSamples)
	assert.False(t, failed[0].Extension)

	// Frame 3: extension re-enabled; recovery must be seamless.
	pktizer.EnableAbsSendTime(1)
	recovered, err := pktizer.Packetize(make([]byte, 40), frameSamples)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	// First frame of recovery: timestamp is the failed frame's timestamp
	// plus the failed frame's sample count; sequence stays consecutive.
	assertFramePackets(t, recovered, 1002, 50000+2*frameSamples)
	assert.True(t, recovered[0].Extension)
	assert.Equal(t, []byte{0x40, 0, 0}, recovered[0].GetExtension(1))

	// Frame 4: one more success proves the state keeps advancing, never
	// rolls back or jumps.
	again, err := pktizer.Packetize(make([]byte, 40), frameSamples)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assertFramePackets(t, again, 1003, 50000+3*frameSamples)

	assert.Equal(t, uint32(50000+4*frameSamples), p.Timestamp)
}

func TestPacketize_EmptyPayload_AdvancesTimestampWithoutSequenceNumbers(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)

	// A normal frame first, so the sequence-number consumption of later
	// frames is observable.
	first, err := pktizer.Packetize(make([]byte, 40), 2000)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assertFramePackets(t, first, 1000, 50000)

	// Empty payload: nil packets and nil error (distinguishable from an
	// extension failure), timestamp moves, no sequence number consumed.
	empty, err := pktizer.Packetize(nil, 1500)
	require.NoError(t, err, "empty payload is not an error")
	assert.Nil(t, empty, "empty payload must return nil packets")
	assert.Equal(t, uint32(50000+2000+1500), p.Timestamp)
	assert.Equal(t, uint16(1000), lastConsumedSequence(t, p),
		"empty payload must not consume a sequence number")

	// Next real frame reuses seq 1001 and the advanced timestamp.
	second, err := pktizer.Packetize(make([]byte, 40), 3000)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assertFramePackets(t, second, 1001, 50000+2000+1500)
}

func TestPacketize_EmptyPayload_WithAbsSendTimeEnabled_StillWritesNothing(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	pktizer.EnableAbsSendTime(15) // would be invalid on a real packet

	empty, err := pktizer.Packetize(nil, 1500)
	require.NoError(t, err, "empty payload must never trigger extension writes")
	assert.Nil(t, empty)
	assert.Equal(t, uint32(50000+1500), p.Timestamp)
	assert.Equal(t, uint16(999), lastConsumedSequence(t, p),
		"empty payload must not consume a sequence number even with AST enabled")
}

func TestPacketize_FailureIsDistinguishableFromEmptyPayload(t *testing.T) {
	_, failing := newTestPacketizer(t, 100)
	failing.EnableAbsSendTime(15)
	failedPackets, failedErr := failing.Packetize(make([]byte, 40), 100)
	require.Error(t, failedErr)
	require.NotEmpty(t, failedPackets, "extension failure returns the frame packets")

	_, empty := newTestPacketizer(t, 100)
	emptyPackets, emptyErr := empty.Packetize(nil, 100)
	require.NoError(t, emptyErr)
	assert.Nil(t, emptyPackets, "empty payload returns nil, nil")
}

func TestPacketize_SequenceWrapsWithoutGap(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	// Start directly below the 16-bit boundary.
	p.Sequencer = NewFixedSequencer(65534)

	first, err := pktizer.Packetize(make([]byte, 40), 100)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, uint16(65534), first[0].SequenceNumber)

	second, err := pktizer.Packetize(make([]byte, 40), 100)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, uint16(65535), second[0].SequenceNumber)

	third, err := pktizer.Packetize(make([]byte, 40), 100)
	require.NoError(t, err)
	require.Len(t, third, 1)
	assert.Equal(t, uint16(0), third[0].SequenceNumber, "sequence number must wrap to 0, not skip")
	assert.Equal(t, uint32(50000+200), third[0].Timestamp, "third frame starts at 50000 + 2 prior frames")
	assert.Equal(t, uint32(50000+300), p.Timestamp, "timestamp keeps advancing across the wrap")
}

func TestPacketize_Concurrent(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	pktizer.EnableAbsSendTime(1)

	const (
		goroutines    = 16
		framesPerG    = 50
		samples       = uint32(960)
		payloadPerPkt = 40 // fits in one G722 payload at MTU 100 -> one packet per frame
	)

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	// Every produced packet lands here; the mutex on the packetizer must
	// guarantee globally unique, consecutive sequence numbers.
	allPackets := make(chan *Packet, goroutines*framesPerG)

	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range framesPerG {
				packets, err := pktizer.Packetize(make([]byte, payloadPerPkt), samples)
				if err != nil {
					errCh <- fmt.Errorf("unexpected packetize error: %w", err)

					return
				}
				if len(packets) != 1 {
					errCh <- fmt.Errorf("expected 1 packet, got %d", len(packets))

					return
				}
				allPackets <- packets[0]
			}
		}()
	}
	close(start)
	wg.Wait()
	close(allPackets)
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	totalFrames := goroutines * framesPerG
	seen := make(map[uint16]bool, totalFrames)
	tsMin := uint32(0)
	tsMax := uint32(0)
	first := true
	count := 0
	for pkt := range allPackets {
		count++
		assert.False(t, seen[pkt.SequenceNumber], "duplicate sequence number %d", pkt.SequenceNumber)
		seen[pkt.SequenceNumber] = true
		// Every packet is the last (and only) packet of its frame, so the
		// marker and the AbsSendTime extension must always be present.
		assert.True(t, pkt.Marker)
		assert.True(t, pkt.Extension, "last packet must carry AbsSendTime")
		assert.NotNil(t, pkt.GetExtension(1))
		if first || pkt.Timestamp < tsMin {
			tsMin = pkt.Timestamp
		}
		if first || pkt.Timestamp > tsMax {
			tsMax = pkt.Timestamp
		}
		first = false
	}
	assert.Equal(t, totalFrames, count)

	// No sequence number was skipped: exactly totalFrames distinct values.
	assert.Len(t, seen, totalFrames)
	// Timestamps only move forward: every frame advanced by exactly `samples`.
	assert.Equal(t, uint32(totalFrames)*samples, tsMax-tsMin+samples,
		"timestamp range must cover every frame exactly once")
	assert.Equal(t, uint32(50000+uint32(totalFrames)*samples), p.Timestamp)
}

func TestPacketize_Concurrent_MixedSuccessFailureAndEmpty(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	pktizer.EnableAbsSendTime(1)

	const (
		goroutines      = 12
		successPerPhase = 10
		realSamples     = uint32(960)
		emptySamples    = uint32(480)
	)

	var (
		wg sync.WaitGroup
		// Barriers make the success/failure/recovery phases deterministic
		// even though all goroutines share one packetizer.
		phaseA = make(chan struct{})
		phaseB = make(chan struct{})
		phaseC = make(chan struct{})
		doneA  sync.WaitGroup
		doneB  sync.WaitGroup
	)
	errCh := make(chan error, goroutines)

	for range goroutines {
		wg.Add(1)
		doneA.Add(1)
		doneB.Add(1)
		go func() {
			defer wg.Done()

			// Phase A: successful frames, with one empty frame that must
			// only advance the timestamp.
			<-phaseA
			for i := range successPerPhase {
				if i == 5 {
					packets, err := pktizer.Packetize(nil, emptySamples)
					if err != nil || packets != nil {
						errCh <- fmt.Errorf("empty frame must be (nil, nil), got (%v, %v)", packets, err)
						doneA.Done()
						doneB.Done()

						return
					}

					continue
				}
				packets, err := pktizer.Packetize(make([]byte, 40), realSamples)
				if err != nil || len(packets) != 1 || !packets[0].Extension {
					errCh <- fmt.Errorf("phase A frame %d: unexpected result (%d packets, %v)", i, len(packets), err)
					doneA.Done()
					doneB.Done()

					return
				}
			}
			doneA.Done()

			// Phase B: exactly one failing frame per goroutine.
			<-phaseB
			packets, err := pktizer.Packetize(make([]byte, 40), realSamples)
			if err == nil || !errors.Is(err, ErrAbsSendTimeExtension) {
				errCh <- fmt.Errorf("phase B: expected wrapped extension error, got %v", err)
				doneB.Done()

				return
			}
			if len(packets) != 1 || packets[0].Extension {
				errCh <- fmt.Errorf("phase B: failed frame must be returned extension-free")
				doneB.Done()

				return
			}
			doneB.Done()

			// Phase C: recovery, all frames succeed again.
			<-phaseC
			for range successPerPhase {
				packets, err := pktizer.Packetize(make([]byte, 40), realSamples)
				if err != nil || len(packets) != 1 || !packets[0].Extension {
					errCh <- fmt.Errorf("phase C: unexpected result (%d packets, %v)", len(packets), err)

					return
				}
			}
		}()
	}

	close(phaseA)
	doneA.Wait()
	// Switch to the reserved extension id only once every goroutine is
	// between phases, so phase B failures are deterministic.
	pktizer.EnableAbsSendTime(15)
	close(phaseB)
	doneB.Wait()
	pktizer.EnableAbsSendTime(1)
	close(phaseC)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	// Per goroutine: 9 phase-A real frames + 1 failing phase-B frame +
	// 10 phase-C frames = 20 real frames, plus 1 empty frame.
	const realFrames = goroutines * 2 * successPerPhase
	const emptyFrames = goroutines
	assert.Equal(t, uint32(50000+uint32(realFrames)*realSamples+uint32(emptyFrames)*emptySamples), p.Timestamp)
	// Only real frames consumed sequence numbers; the counter is continuous.
	assert.Equal(t, uint16(999+realFrames), lastConsumedSequence(t, p)) // nolint: gosec // G115
}

func TestWriteAbsSendTime_LeavesPacketUntouchedOnError(t *testing.T) {
	p, pktizer := newTestPacketizer(t, 100)
	pktizer.EnableAbsSendTime(15)

	packet := &Packet{
		Header: Header{
			Version:        2,
			Marker:         true,
			PayloadType:    98,
			SequenceNumber: 1234,
			Timestamp:      50000,
			SSRC:           0x1234ABCD,
		},
		Payload: []byte{1, 2, 3, 4},
	}

	err := p.writeAbsSendTime(packet)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errRFC8285OneByteHeaderIDRange))
	assert.False(t, packet.Extension, "errored write must not mutate the packet")
	assert.Zero(t, packet.ExtensionProfile)
	assert.Empty(t, packet.Extensions)
}
