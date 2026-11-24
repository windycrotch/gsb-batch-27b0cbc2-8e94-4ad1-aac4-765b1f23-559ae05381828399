package volume

import (
	"math"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// newDefaultBackend returns the Windows audio backend backed by the Core
// Audio APIs through COM.
func newDefaultBackend() audioBackend {
	return &windowsBackend{}
}

type windowsBackend struct{}

func (b *windowsBackend) Status() (State, error) {
	ret, err := invoke(func(aev *wca.IAudioEndpointVolume) (interface{}, error) {
		var level float32
		if err := aev.GetMasterVolumeLevelScalar(&level); err != nil {
			return nil, err
		}
		volume := int(math.Floor(float64(level*100.0 + 0.5)))
		var muted bool
		if err := aev.GetMute(&muted); err != nil {
			return nil, err
		}
		return State{Volume: volume, Muted: muted}, nil
	})
	if err != nil {
		return State{}, err
	}
	return ret.(State), nil
}

func (b *windowsBackend) SetVolume(volume int) error {
	_, err := invoke(func(aev *wca.IAudioEndpointVolume) (interface{}, error) {
		err := aev.SetMasterVolumeLevelScalar(float32(volume)/100, nil)
		return nil, err
	})
	return err
}

func (b *windowsBackend) SetMuted(muted bool) error {
	_, err := invoke(func(aev *wca.IAudioEndpointVolume) (interface{}, error) {
		err := aev.SetMute(muted, nil)
		return nil, err
	})
	return err
}

func invoke(f func(aev *wca.IAudioEndpointVolume) (interface{}, error)) (ret interface{}, err error) {
	if err = ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		return
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err = wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return
	}
	defer mmde.Release()

	var mmd *wca.IMMDevice
	if err = mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &mmd); err != nil {
		return
	}
	defer mmd.Release()

	var ps *wca.IPropertyStore
	if err = mmd.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return
	}
	defer ps.Release()

	var pv wca.PROPVARIANT
	if err = ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return
	}

	var aev *wca.IAudioEndpointVolume
	if err = mmd.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
		return
	}
	defer aev.Release()

	ret, err = f(aev)
	return
}
