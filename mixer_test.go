package oss_test

import (
	"testing"

	"github.com/gen2brain/oss"
)

func TestMixer(t *testing.T) {
	mix, err := oss.OpenMixer()
	if err != nil {
		t.Skipf("no usable mixer device: %v", err)
	}
	defer mix.Close()

	_, err = mix.Controls()
	if err != nil {
		t.Error(err)
	}

	ok, err := mix.IsControl(oss.SoundMixerVolume)
	if err != nil {
		t.Error(err)
	}

	if !ok {
		t.Skip("mixer has no master volume control")
	}

	reqLeft, reqRight := 50, 50
	curLeft, curRight, err := mix.Volume(oss.SoundMixerVolume)
	if err != nil {
		t.Error(err)
	}

	left, right, err := mix.SetVolume(oss.SoundMixerVolume, reqLeft, reqRight)
	if err != nil {
		t.Error(err)
	}

	if left != reqLeft || right != reqRight {
		t.Errorf("SetVolume: got %d,%d want %d,%d", left, right, reqLeft, reqRight)
	}

	_, _, err = mix.SetVolume(oss.SoundMixerVolume, curLeft, curRight)
	if err != nil {
		t.Error(err)
	}
}
