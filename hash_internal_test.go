package gotau

import (
	"bytes"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/SladkyCitron/gotau/concat"
	"github.com/SladkyCitron/gotau/resampler"
	"github.com/SladkyCitron/gotau/sequence"
	"github.com/SladkyCitron/gotau/voicebank"
	"github.com/SladkyCitron/resona/afmt"
	"github.com/SladkyCitron/resona/aio"
	"github.com/SladkyCitron/resona/freq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type idResampler struct{ id string }

func (r *idResampler) ID() string { return r.id }

func (r *idResampler) Resample(_ aio.SampleReader, _ resampler.ResampleConfig) (aio.SampleReader, error) {
	return nil, nil
}

func testKeyFormat(sr int) afmt.Format {
	return afmt.Format{SampleRate: freq.Frequency(sr) * freq.Hertz, NumChannels: 1}
}

func TestCacheKeyDistinguishesConfigurations(t *testing.T) {
	mfs := fstest.MapFS{"sample.wav": &fstest.MapFile{Data: []byte("x")}}
	vb, err := voicebank.Open(mfs)
	require.NoError(t, err)
	fileinfo, err := fs.Stat(mfs, "sample.wav")
	require.NoError(t, err)

	keyFor := func(resID string, cfg resampler.ResampleConfig, path string) []byte {
		s := New(44100, vb, &idResampler{id: resID}, &concat.Append{})
		var buf bytes.Buffer
		s.getKeyFunc(cfg, path, fileinfo)(&buf)
		return buf.Bytes()
	}

	base := resampler.ResampleConfig{
		Pitch:       60,
		Velocity:    100,
		Length:      250,
		Offset:      18,
		AudioFormat: testKeyFormat(44100),
	}

	assert.Equal(t, keyFor("res-a", base, "sample.wav"), keyFor("res-a", base, "sample.wav"),
		"the key must be deterministic")

	assert.NotEqual(t, keyFor("res-a", base, "sample.wav"), keyFor("res-b", base, "sample.wav"),
		"different resamplers must not share a key")

	otherPitch := base
	otherPitch.Pitch = 61
	assert.NotEqual(t, keyFor("res-a", base, "sample.wav"), keyFor("res-a", otherPitch, "sample.wav"),
		"different pitch must not share a key")

	otherLength := base
	otherLength.Length = 300
	assert.NotEqual(t, keyFor("res-a", base, "sample.wav"), keyFor("res-a", otherLength, "sample.wav"),
		"different length must not share a key")

	otherRate := base
	otherRate.AudioFormat = testKeyFormat(48000)
	assert.NotEqual(t, keyFor("res-a", base, "sample.wav"), keyFor("res-a", otherRate, "sample.wav"),
		"different sample rate must not share a key")

	otherFlags := base
	otherFlags.Flags = sequence.Flags{"B": 10}
	assert.NotEqual(t, keyFor("res-a", base, "sample.wav"), keyFor("res-a", otherFlags, "sample.wav"),
		"different flags must not share a key")

	otherBend := base
	otherBend.PitchBend = []float64{0, 10}
	assert.NotEqual(t, keyFor("res-a", base, "sample.wav"), keyFor("res-a", otherBend, "sample.wav"),
		"different pitch bend must not share a key")

	// Without length-prefixing, shifting bytes between adjacent string fields
	// could produce identical keys. Make sure it does not.
	assert.NotEqual(t, keyFor("ab", base, "c"), keyFor("a", base, "bc"),
		"adjacent string fields must not be able to collide")
}
