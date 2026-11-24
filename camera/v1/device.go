package camera

import (
	"sync"

	"github.com/Kirizu-Official/windows-camera-go/utils"
	"github.com/Kirizu-Official/windows-camera-go/windows/consts"
	"github.com/Kirizu-Official/windows-camera-go/windows/mf"
)

// sourceReader is the subset of *mf.IMFSourceReader that the capture session
// actually needs. Keeping it an interface lets the capture logic be driven with
// an in-process fake in tests, without a real camera or any COM calls.
type sourceReader interface {
	SetCurrentMediaType(dwStreamIndex uint32, pType *mf.IMFMediaType) error
	ReadSample(dwStreamIndex uint32, dwControlFlags uint32, pdwActualStreamIndex *uint32, pdwStreamFlags *uint32, pllTimestamp *int64, ppSample **mf.IMFSample) error
	Release() error
}

// sourceReaderProvider decouples capture startup from the concrete Media
// Foundation factory, again so tests can substitute an in-process provider.
type sourceReaderProvider interface {
	createSourceReader(config *CaptureConfig) (sourceReader, *mf.IMFMediaType, error)
}

type Device struct {
	Name       string
	SymbolLink string

	// mu protects every mutable field below, including the closed flag, the
	// reader pointers and the error callback.
	mu sync.Mutex

	pDeviceActive *mf.IMFActivate
	pDeviceSource *mf.IMFMediaSource
	pDevPresent   *mf.IMFPresentationDescriptor
	pSourceReader *mf.IMFSourceReader
	pMediaType    *mf.IMFMediaType

	// reader is the interface used by both capture paths and by CloseDevice.
	// In production it is the same object as pSourceReader; in tests it can be
	// a fake.
	reader sourceReader
	// readerProvider builds the source reader at capture startup.
	readerProvider sourceReaderProvider

	config        *CaptureConfig
	errorCallBack func(err error)

	// closed is set first during shutdown, before any resource is released, so
	// that late callbacks and frame requests fail fast instead of touching
	// released objects.
	closed bool
	// closeOnce makes sure concurrent or repeated CloseDevice calls converge on
	// a single actual release.
	closeOnce sync.Once
	// callbackWG tracks callbacks currently executing. CloseDevice waits for
	// all of them to leave before releasing anything they might touch.
	callbackWG sync.WaitGroup
}

func OpenDevice(symbolLink string) (*Device, error) {

	sources, err := callEnumDeviceSources()
	if err != nil {
		return nil, err
	}

	var info *DeviceInfo
	for _, source := range sources {
		info, err = callGetBaseInfo(source)
		if err != nil {
			continue
		}
		if info.SymbolLink == symbolLink {
			device := &Device{
				Name:          info.Name,
				SymbolLink:    info.SymbolLink,
				pDeviceActive: source,
			}
			err = device.init()
			if err != nil {
				return nil, err
			}
			return device, nil
		} else {
			_ = source.Release()
		}
	}
	return nil, utils.ErrorDeviceNotFound
}

func (d *Device) init() error {
	if d.pDeviceActive == nil {
		return utils.ErrorDeviceNotAllowedCall
	}

	err := d.pDeviceActive.ActivateObject(&d.pDeviceSource)
	if err != nil {
		return err
	}

	err = d.pDeviceSource.CreatePresentationDescriptor(&d.pDevPresent)
	if err != nil {
		return err
	}

	d.readerProvider = &comSourceReaderProvider{device: d}
	return nil
}

// beginCallback marks one callback as in flight. It returns false once the
// device has started closing, in which case the caller must return immediately
// without touching any device resource.
func (d *Device) beginCallback() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	d.callbackWG.Add(1)
	return true
}

func (d *Device) endCallback() {
	d.callbackWG.Done()
}

// isClosed reports whether the device has started (or finished) closing.
func (d *Device) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// SetErrorCallBack registers the handler invoked for asynchronous capture
// errors. It is safe to call concurrently with error delivery.
func (d *Device) SetErrorCallBack(fn func(err error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.errorCallBack = fn
}

func (d *Device) GetControl() (*mf.IAMCameraControl, *mf.IAMVideoProcAmp, error) {
	d.mu.Lock()
	reader := d.pSourceReader
	closed := d.closed
	d.mu.Unlock()
	if closed || reader == nil {
		return nil, nil, utils.ErrorDeviceNotOpen
	}
	return reader.GetServiceForStream()
}

type ConfigParameterStatus struct {
	Current  int32 `json:"current" yaml:"current"`
	Min      int32 `json:"min" yaml:"min"`
	Max      int32 `json:"max" yaml:"max"`
	Default  int32 `json:"default" yaml:"default"`
	Flags    int32 `json:"flags" yaml:"flags"`       // Flags indicate the control
	Stepping int32 `json:"stepping" yaml:"stepping"` // Setting indicates the current setting of the control
	Found    bool  `json:"found" yaml:"found"`       // Found indicates if the control was found
}
type ConfigParameter struct {
	Pan                   ConfigParameterStatus `json:"pan" yaml:"pan"`
	Tilt                  ConfigParameterStatus `json:"tilt" yaml:"tilt"`
	Roll                  ConfigParameterStatus `json:"roll" yaml:"roll"`
	Zoom                  ConfigParameterStatus `json:"zoom" yaml:"zoom"`
	Exposure              ConfigParameterStatus `json:"exposure" yaml:"exposure"`
	Iris                  ConfigParameterStatus `json:"iris" yaml:"iris"`
	Focus                 ConfigParameterStatus `json:"focus" yaml:"focus"`
	Brightness            ConfigParameterStatus `json:"brightness" yaml:"brightness"`
	Contrast              ConfigParameterStatus `json:"contrast" yaml:"contrast"`
	Hue                   ConfigParameterStatus `json:"hue" yaml:"hue"`
	Saturation            ConfigParameterStatus `json:"saturation" yaml:"saturation"`
	Sharpness             ConfigParameterStatus `json:"sharpness" yaml:"sharpness"`
	Gamma                 ConfigParameterStatus `json:"gamma" yaml:"gamma"`
	ColorEnable           ConfigParameterStatus `json:"color_enable" yaml:"color_enable"`
	WhiteBalance          ConfigParameterStatus `json:"white_balance" yaml:"white_balance"`
	BacklightCompensation ConfigParameterStatus `json:"backlight_compensation" yaml:"backlight_compensation"`
	Gain                  ConfigParameterStatus `json:"gain" yaml:"gain"`
}

func (d *Device) getConfig() (*ConfigParameter, error) {
	cameraCtl, procAmp, err := d.GetControl()
	if err != nil {
		return nil, err
	}
	pan, err := cameraCtl.GetRange(consts.CameraControl_Pan)
	if err != nil {
		return nil, err
	}

	tilt, err := cameraCtl.GetRange(consts.CameraControl_Tilt)
	if err != nil {
		return nil, err
	}
	roll, err := cameraCtl.GetRange(consts.CameraControl_Roll)
	if err != nil {
		return nil, err
	}
	zoom, err := cameraCtl.GetRange(consts.CameraControl_Zoom)
	if err != nil {
		return nil, err
	}
	exposure, err := cameraCtl.GetRange(consts.CameraControl_Exposure)
	if err != nil {
		return nil, err
	}
	iris, err := cameraCtl.GetRange(consts.CameraControl_Iris)
	if err != nil {
		return nil, err
	}
	focus, err := cameraCtl.GetRange(consts.CameraControl_Focus)
	if err != nil {
		return nil, err
	}
	brightness, err := procAmp.GetRange(consts.VideoProcAmp_Brightness)
	if err != nil {
		return nil, err
	}
	contrast, err := procAmp.GetRange(consts.VideoProcAmp_Contrast)
	if err != nil {
		return nil, err
	}
	hue, err := procAmp.GetRange(consts.VideoProcAmp_Hue)
	if err != nil {
		return nil, err
	}
	saturation, err := procAmp.GetRange(consts.VideoProcAmp_Saturation)
	if err != nil {
		return nil, err
	}
	sharpness, err := procAmp.GetRange(consts.VideoProcAmp_Sharpness)
	if err != nil {
		return nil, err
	}
	gamma, err := procAmp.GetRange(consts.VideoProcAmp_Gamma)
	if err != nil {
		return nil, err
	}
	colorEnable, err := procAmp.GetRange(consts.VideoProcAmp_ColorEnable)
	if err != nil {
		return nil, err
	}
	whiteBalance, err := procAmp.GetRange(consts.VideoProcAmp_WhiteBalance)
	if err != nil {
		return nil, err
	}
	backlightCompensation, err := procAmp.GetRange(consts.VideoProcAmp_BacklightCompensation)
	if err != nil {
		return nil, err
	}
	gain, err := procAmp.GetRange(consts.VideoProcAmp_Gain)
	if err != nil {
		return nil, err
	}
	panCurrent, _, err := cameraCtl.Get(consts.CameraControl_Pan)
	if err != nil {
		return nil, err
	}
	tiltCurrent, _, err := cameraCtl.Get(consts.CameraControl_Tilt)
	if err != nil {
		return nil, err
	}
	rollCurrent, _, err := cameraCtl.Get(consts.CameraControl_Roll)
	if err != nil {
		return nil, err
	}
	zoomCurrent, _, err := cameraCtl.Get(consts.CameraControl_Zoom)
	if err != nil {
		return nil, err
	}
	exposureCurrent, _, err := cameraCtl.Get(consts.CameraControl_Exposure)
	if err != nil {
		return nil, err
	}
	irisCurrent, _, err := cameraCtl.Get(consts.CameraControl_Iris)
	if err != nil {
		return nil, err
	}
	focusCurrent, _, err := cameraCtl.Get(consts.CameraControl_Focus)
	if err != nil {
		return nil, err
	}
	brightnessCurrent, _, err := procAmp.Get(consts.VideoProcAmp_Brightness)
	if err != nil {
		return nil, err
	}
	contrastCurrent, _, err := procAmp.Get(consts.VideoProcAmp_Contrast)
	if err != nil {
		return nil, err
	}
	hueCurrent, _, err := procAmp.Get(consts.VideoProcAmp_Hue)
	if err != nil {
		return nil, err
	}
	saturationCurrent, _, err := procAmp.Get(consts.VideoProcAmp_Saturation)
	if err != nil {
		return nil, err
	}
	sharpnessCurrent, _, err := procAmp.Get(consts.VideoProcAmp_Sharpness)
	if err != nil {
		return nil, err
	}
	gammaCurrent, _, err := procAmp.Get(consts.VideoProcAmp_Gamma)
	if err != nil {
		return nil, err
	}
	colorEnableCurrent, _, err := procAmp.Get(consts.VideoProcAmp_ColorEnable)
	if err != nil {
		return nil, err
	}
	whiteBalanceCurrent, _, err := procAmp.Get(consts.VideoProcAmp_WhiteBalance)
	if err != nil {
		return nil, err
	}
	backlightCompensationCurrent, _, err := procAmp.Get(consts.VideoProcAmp_BacklightCompensation)
	if err != nil {
		return nil, err
	}
	gainCurrent, _, err := procAmp.Get(consts.VideoProcAmp_Gain)
	if err != nil {
		return nil, err
	}
	panStatus := ConfigParameterStatus{
		Current:  panCurrent,
		Min:      pan.Min,
		Max:      pan.Max,
		Default:  pan.DefaultValue,
		Flags:    int32(pan.CapsFlags),
		Stepping: pan.SteppingDelta,
		Found:    pan.Found,
	}
	tiltStatus := ConfigParameterStatus{
		Current:  tiltCurrent,
		Min:      tilt.Min,
		Max:      tilt.Max,
		Default:  tilt.DefaultValue,
		Flags:    int32(tilt.CapsFlags),
		Stepping: tilt.SteppingDelta,
		Found:    tilt.Found,
	}
	rollStatus := ConfigParameterStatus{
		Current:  rollCurrent,
		Min:      roll.Min,
		Max:      roll.Max,
		Default:  roll.DefaultValue,
		Flags:    int32(roll.CapsFlags),
		Stepping: roll.SteppingDelta,
		Found:    roll.Found,
	}
	zoomStatus := ConfigParameterStatus{
		Current:  zoomCurrent,
		Min:      zoom.Min,
		Max:      zoom.Max,
		Default:  zoom.DefaultValue,
		Flags:    int32(zoom.CapsFlags),
		Stepping: zoom.SteppingDelta,
		Found:    zoom.Found,
	}
	exposureStatus := ConfigParameterStatus{
		Current:  exposureCurrent,
		Min:      exposure.Min,
		Max:      exposure.Max,
		Default:  exposure.DefaultValue,
		Flags:    int32(exposure.CapsFlags),
		Stepping: exposure.SteppingDelta,
		Found:    exposure.Found,
	}
	irisStatus := ConfigParameterStatus{
		Current:  irisCurrent,
		Min:      iris.Min,
		Max:      iris.Max,
		Default:  iris.DefaultValue,
		Flags:    int32(iris.CapsFlags),
		Stepping: iris.SteppingDelta,
		Found:    iris.Found,
	}
	focusStatus := ConfigParameterStatus{
		Current:  focusCurrent,
		Min:      focus.Min,
		Max:      focus.Max,
		Default:  focus.DefaultValue,
		Flags:    int32(focus.CapsFlags),
		Stepping: focus.SteppingDelta,
		Found:    focus.Found,
	}
	brightnessStatus := ConfigParameterStatus{
		Current:  brightnessCurrent,
		Min:      brightness.Min,
		Max:      brightness.Max,
		Default:  brightness.DefaultValue,
		Flags:    int32(brightness.CapsFlags),
		Stepping: brightness.SteppingDelta,
		Found:    brightness.Found,
	}
	contrastStatus := ConfigParameterStatus{
		Current:  contrastCurrent,
		Min:      contrast.Min,
		Max:      contrast.Max,
		Default:  contrast.DefaultValue,
		Flags:    int32(contrast.CapsFlags),
		Stepping: contrast.SteppingDelta,
		Found:    contrast.Found,
	}
	hueStatus := ConfigParameterStatus{
		Current:  hueCurrent,
		Min:      hue.Min,
		Max:      hue.Max,
		Default:  hue.DefaultValue,
		Flags:    int32(hue.CapsFlags),
		Stepping: hue.SteppingDelta,
		Found:    hue.Found,
	}
	saturationStatus := ConfigParameterStatus{
		Current:  saturationCurrent,
		Min:      saturation.Min,
		Max:      saturation.Max,
		Default:  saturation.DefaultValue,
		Flags:    int32(saturation.CapsFlags),
		Stepping: saturation.SteppingDelta,
		Found:    saturation.Found,
	}
	sharpnessStatus := ConfigParameterStatus{
		Current:  sharpnessCurrent,
		Min:      sharpness.Min,
		Max:      sharpness.Max,
		Default:  sharpness.DefaultValue,
		Flags:    int32(sharpness.CapsFlags),
		Stepping: sharpness.SteppingDelta,
		Found:    sharpness.Found,
	}
	gammaStatus := ConfigParameterStatus{
		Current:  gammaCurrent,
		Min:      gamma.Min,
		Max:      gamma.Max,
		Default:  gamma.DefaultValue,
		Flags:    int32(gamma.CapsFlags),
		Stepping: gamma.SteppingDelta,
		Found:    gamma.Found,
	}
	colorEnableStatus := ConfigParameterStatus{
		Current:  colorEnableCurrent,
		Min:      colorEnable.Min,
		Max:      colorEnable.Max,
		Default:  colorEnable.DefaultValue,
		Flags:    int32(colorEnable.CapsFlags),
		Stepping: colorEnable.SteppingDelta,
		Found:    colorEnable.Found,
	}
	whiteBalanceStatus := ConfigParameterStatus{
		Current:  whiteBalanceCurrent,
		Min:      whiteBalance.Min,
		Max:      whiteBalance.Max,
		Default:  whiteBalance.DefaultValue,
		Flags:    int32(whiteBalance.CapsFlags),
		Stepping: whiteBalance.SteppingDelta,
		Found:    whiteBalance.Found,
	}
	backlightCompensationStatus := ConfigParameterStatus{
		Current:  backlightCompensationCurrent,
		Min:      backlightCompensation.Min,
		Max:      backlightCompensation.Max,
		Default:  backlightCompensation.DefaultValue,
		Flags:    int32(backlightCompensation.CapsFlags),
		Stepping: backlightCompensation.SteppingDelta,
		Found:    backlightCompensation.Found,
	}
	gainStatus := ConfigParameterStatus{
		Current:  gainCurrent,
		Min:      gain.Min,
		Max:      gain.Max,
		Default:  gain.DefaultValue,
		Flags:    int32(gain.CapsFlags),
		Stepping: gain.SteppingDelta,
		Found:    gain.Found,
	}

	return &ConfigParameter{
		Pan:                   panStatus,
		Tilt:                  tiltStatus,
		Roll:                  rollStatus,
		Zoom:                  zoomStatus,
		Exposure:              exposureStatus,
		Iris:                  irisStatus,
		Focus:                 focusStatus,
		Brightness:            brightnessStatus,
		Contrast:              contrastStatus,
		Hue:                   hueStatus,
		Saturation:            saturationStatus,
		Sharpness:             sharpnessStatus,
		Gamma:                 gammaStatus,
		ColorEnable:           colorEnableStatus,
		WhiteBalance:          whiteBalanceStatus,
		BacklightCompensation: backlightCompensationStatus,
		Gain:                  gainStatus,
	}, nil

}

// CloseDevice ends the capture session and releases every underlying COM
// resource. It is idempotent and safe to call concurrently: only the first
// call performs the release, every subsequent call returns immediately.
//
// The ordering is deliberate:
//  1. Flip the closed flag while holding the lock so no new callback or frame
//     request can start (they fail with ErrorDeviceNotOpen instead).
//  2. Wait for callbacks already in flight to finish, so none of them can
//     dereference an object released below.
//  3. Release source reader -> media type -> presentation descriptor -> media
//     source -> activate object, each exactly once.
func (d *Device) CloseDevice() {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		reader := d.reader
		pSourceReader := d.pSourceReader
		pMediaType := d.pMediaType
		pDevPresent := d.pDevPresent
		pDeviceSource := d.pDeviceSource
		pDeviceActive := d.pDeviceActive
		d.mu.Unlock()

		// Wait outside the lock: in-flight callbacks do not hold d.mu while
		// doing their work, so they can always reach endCallback().
		d.callbackWG.Wait()

		// Prefer the interface handle (present in both production and test
		// builds); fall back to the concrete pointer only if the interface was
		// never assigned. Each pointer is released once and then dropped.
		if reader != nil {
			_ = reader.Release()
		} else if pSourceReader != nil {
			_ = pSourceReader.Release()
		}
		if pMediaType != nil {
			_ = pMediaType.Release()
		}
		if pDevPresent != nil {
			_ = pDevPresent.Release()
		}
		if pDeviceSource != nil {
			_ = pDeviceSource.Release()
		}
		if pDeviceActive != nil {
			_ = pDeviceActive.Release()
		}

		d.mu.Lock()
		d.reader = nil
		d.pSourceReader = nil
		d.pMediaType = nil
		d.pDevPresent = nil
		d.pDeviceSource = nil
		d.pDeviceActive = nil
		d.config = nil
		d.mu.Unlock()
	})
}
