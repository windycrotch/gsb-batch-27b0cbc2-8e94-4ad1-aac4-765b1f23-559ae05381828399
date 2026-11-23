package gotau

import (
	"encoding/binary"
	"io"
	"io/fs"

	"github.com/SladkyCitron/gotau/cache"
	"github.com/SladkyCitron/gotau/resampler"
)

// keyVersion is bumped whenever the layout of the cache key changes, so that
// stale entries produced by an older key layout are never reused.
const keyVersion uint64 = 2

// writeKeyString writes a length-prefixed string to the key.
//
// The length prefix is important: without it, variable-length fields (most
// notably the resampler ID and the voicebank path) could be shifted around and
// produce the same byte stream for different inputs.
func writeKeyString(w io.Writer, s string) {
	_ = binary.Write(w, binary.LittleEndian, uint64(len(s)))
	_, _ = io.WriteString(w, s)
}

// writeKeyBytes writes a length-prefixed byte slice to the key.
func writeKeyBytes(w io.Writer, b []byte) {
	_ = binary.Write(w, binary.LittleEndian, uint64(len(b)))
	_, _ = w.Write(b)
}

// getKeyFunc returns a [cache.KeyFunc] that uniquely identifies the resampled
// audio produced from the given configuration and voicebank sample.
//
// The key covers the resampler identity, all resampling parameters, the input
// file identity (path, size, modification time), and the output audio format.
// Different resamplers or different parameters therefore never share a cache
// entry, and changing the sample rate or channel count invalidates old entries.
func (s *Synth) getKeyFunc(cfg resampler.ResampleConfig, path string, fileinfo fs.FileInfo) cache.KeyFunc {
	mtime, err := fileinfo.ModTime().MarshalBinary()
	if err != nil {
		mtime = []byte{}
	}

	flags, err := cfg.Flags.MarshalBinary()
	if err != nil {
		flags = []byte{}
	}

	resamplerID := s.res.ID()

	return func(w io.Writer) {
		writeKeyString(w, "gotau-resampler")
		_ = binary.Write(w, binary.LittleEndian, keyVersion)
		writeKeyString(w, resamplerID)
		writeKeyString(w, path)
		_ = binary.Write(w, binary.LittleEndian, uint64(fileinfo.Size()))
		writeKeyBytes(w, mtime)
		_ = binary.Write(w, binary.LittleEndian, int64(cfg.AudioFormat.SampleRate))
		_ = binary.Write(w, binary.LittleEndian, int64(cfg.AudioFormat.NumChannels))
		_, _ = w.Write([]byte{byte(cfg.Pitch)})
		_ = binary.Write(w, binary.LittleEndian, cfg.Velocity)
		writeKeyBytes(w, flags)
		_ = binary.Write(w, binary.LittleEndian, cfg.Offset)
		_ = binary.Write(w, binary.LittleEndian, cfg.Length)
		_ = binary.Write(w, binary.LittleEndian, cfg.Consonant)
		_ = binary.Write(w, binary.LittleEndian, cfg.Cutoff)
		_ = binary.Write(w, binary.LittleEndian, cfg.Intensity)
		_ = binary.Write(w, binary.LittleEndian, cfg.Modulation)
		_ = binary.Write(w, binary.LittleEndian, cfg.Tempo)
		_ = binary.Write(w, binary.LittleEndian, uint64(len(cfg.PitchBend)))
		_ = binary.Write(w, binary.LittleEndian, cfg.PitchBend)
	}
}
