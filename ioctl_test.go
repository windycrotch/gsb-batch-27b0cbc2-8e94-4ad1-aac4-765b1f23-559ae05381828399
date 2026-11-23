package oss

import "testing"

// TestIoctlNumbers checks the computed ioctl numbers against the values from the
// system <sys/soundcard.h> macros (wantIoctl, per encoding family by build tag).
func TestIoctlNumbers(t *testing.T) {
	got := map[string]uint{
		"DSP_RESET":          sndctlDspReset,
		"DSP_SYNC":           sndctlDspSync,
		"DSP_SPEED":          sndctlDspSpeed,
		"DSP_SETFMT":         sndctlDspSetfmt,
		"DSP_CHANNELS":       sndctlDspChannels,
		"DSP_POST":           sndctlDspPost,
		"DSP_SETFRAGMENT":    sndctlDspSetfragment,
		"DSP_GETFMTS":        sndctlDspGetfmts,
		"DSP_GETOSPACE":      sndctlDspGetospace,
		"DSP_GETISPACE":      sndctlDspGetispace,
		"DSP_NONBLOCK":       sndctlDspNonblock,
		"DSP_GETCAPS":        sndctlDspGetcaps,
		"DSP_SETTRIGGER":     sndctlDspSettrigger,
		"DSP_SETDUPLEX":      sndctlDspSetduplex,
		"DSP_GETODELAY":      sndctlDspGetodelay,
		"MIXER_READ_DEVMASK": soundMixerReadDevmask,
		"MIXER_READ_VOLUME":  soundMixerRead(SoundMixerVolume),
		"MIXER_WRITE_VOLUME": soundMixerWrite(SoundMixerVolume),
	}

	if len(got) != len(wantIoctl) {
		t.Fatalf("ioctl count: got %d, want %d", len(got), len(wantIoctl))
	}

	for name, want := range wantIoctl {
		if int32(got[name]) != want {
			t.Errorf("%s = %d (0x%08x), want %d", name, int32(got[name]), got[name], want)
		}
	}
}
