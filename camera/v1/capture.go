package camera

import (
	"runtime"
	"sync"

	"github.com/Kirizu-Official/windows-camera-go/utils"
	"github.com/Kirizu-Official/windows-camera-go/windows/consts"
	"github.com/Kirizu-Official/windows-camera-go/windows/mf"
)

// maxEmptySampleRetries bounds how many times the asynchronous path re-issues
// a read request after receiving an empty sample (e.g. a stream tick). Beyond
// this the session reports one error and stops re-requesting until the user
// starts a fresh capture.
const maxEmptySampleRetries = 5

type CaptureConfig struct {
	enableAsync bool
	callBack    uintptr
	MediaType   *CaptureFormats
}
type CaptureAsync struct {
	Device   *Device
	Config   *CaptureConfig
	callback *SourceReaderCallback

	// mu guards the fields below.
	mu sync.Mutex
	// emptySampleCount counts consecutive empty samples since the last real
	// sample (or the start of the session).
	emptySampleCount int
	// stopped is set once the empty-sample retry budget is exhausted. While
	// stopped no further read request is issued automatically; it is cleared
	// only by starting a new capture session.
	stopped bool
	// reportOnce makes sure a failed session surfaces at most one error to the
	// device error callback.
	reportOnce sync.Once
}
type CaptureSync struct {
	Device *Device
	Config *CaptureConfig
}

func (d *Device) StartCaptureAsync(mediaType *CaptureFormats) (*CaptureAsync, error) {
	if mediaType == nil {
		return nil, utils.ErrorParameterInvalid
	}
	a := &CaptureAsync{
		Device: d,
	}
	a.callback = newSourceReaderCallback(a)
	config := &CaptureConfig{
		enableAsync: true,
		callBack:    a.callback.ToUintptr(),
		MediaType:   mediaType,
	}
	a.Config = config
	if err := d.startCapture(config); err != nil {
		return nil, err
	}

	return a, nil
}
func (d *Device) StartCapture(mediaType *CaptureFormats) (*CaptureSync, error) {
	if mediaType == nil {
		return nil, utils.ErrorParameterInvalid
	}
	config := &CaptureConfig{
		enableAsync: false,
		MediaType:   mediaType,
	}
	if err := d.startCapture(config); err != nil {
		return nil, err
	}
	return &CaptureSync{
		Device: d,
		Config: config,
	}, nil
}

func (d *Device) startCapture(config *CaptureConfig) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return utils.ErrorDeviceNotOpen
	}
	if d.reader != nil || d.config != nil {
		d.mu.Unlock()
		return utils.ErrorCaptureAlreadyStarted
	}
	provider := d.readerProvider
	d.mu.Unlock()
	if provider == nil {
		return utils.ErrorDeviceNotOpen
	}

	reader, mediaType, err := provider.createSourceReader(config)
	if err != nil {
		return err
	}

	d.mu.Lock()
	// Re-check under the lock: a concurrent CloseDevice may have run while the
	// reader was being created. If so, release what we just built and fail.
	if d.closed {
		d.mu.Unlock()
		_ = reader.Release()
		if mediaType != nil {
			_ = mediaType.Release()
		}
		return utils.ErrorDeviceNotOpen
	}
	d.reader = reader
	if concrete, ok := reader.(*mf.IMFSourceReader); ok {
		d.pSourceReader = concrete
	}
	d.pMediaType = mediaType
	d.config = config
	d.mu.Unlock()
	return nil
}

//func (d *Device) StartCapture() error {
//	fmt.Println(d.pDeviceSource.S, "start capture")
//	if d.config.EnableAsync {
//		err := d.pSourceReader.ReadSampleASync(consts.MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0)
//		if err != nil {
//			return err
//		}
//	} else {
//		var pdwActualStreamIndex, pdwStreamFlags uint32
//		var pllTimestamp int64
//		var ppSample *mf.IMFSample
//
//		err := d.pSourceReader.ReadSample(consts.MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0, &pdwActualStreamIndex, &pdwStreamFlags, &pllTimestamp, &ppSample)
//		if err != nil {
//			return err
//		}
//	}
//
//	return nil
//}

func (d *CaptureAsync) GetCallBackInterface() *SourceReaderCallback {
	return d.callback
}

type SampleData struct {
	PdwActualStreamIndex uint32
	PdwStreamFlags       consts.MF_SOURCE_READER_FLAG
	PllTimestamp         int64
	PpSample             *mf.IMFSample
}

func (s *SampleData) Release() error {
	if s.PpSample != nil {
		err := s.PpSample.Release()
		s.PpSample = nil
		return err
	}
	return nil

}

// GetFrame synchronously reads one real frame. A stream tick means "no new
// sample yet", so it is never surfaced as an error or an empty SampleData: the
// call keeps waiting until an actual sample arrives, a real read error occurs,
// or the device is closed (ErrorDeviceNotOpen).
func (d *CaptureSync) GetFrame() (*SampleData, error) {
	var pdwActualStreamIndex, pdwStreamFlags uint32
	var pllTimestamp int64
	var ppSample *mf.IMFSample

	for {
		if d.Device.isClosed() {
			return nil, utils.ErrorDeviceNotOpen
		}

		d.Device.mu.Lock()
		reader := d.Device.reader
		d.Device.mu.Unlock()
		if reader == nil {
			return nil, utils.ErrorDeviceNotOpen
		}

		err := reader.ReadSample(consts.MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0, &pdwActualStreamIndex, &pdwStreamFlags, &pllTimestamp, &ppSample)
		if err != nil {
			if d.Device.isClosed() {
				return nil, utils.ErrorDeviceNotOpen
			}
			return nil, err
		}

		// The device may have been closed while the read was blocked. Never
		// surface a frame from a closing/closed session.
		if d.Device.isClosed() {
			if ppSample != nil {
				_ = ppSample.Release()
			}
			return nil, utils.ErrorDeviceNotOpen
		}

		flags := consts.MF_SOURCE_READER_FLAG(pdwStreamFlags)
		if flags&consts.MF_SOURCE_READERF_STREAMTICK != 0 || ppSample == nil {
			// Stream tick (or an empty read with no error): keep waiting for a
			// real sample rather than handing back an empty/error result.
			if ppSample != nil {
				_ = ppSample.Release()
				ppSample = nil
			}
			runtime.Gosched()
			continue
		}

		return &SampleData{
			PdwActualStreamIndex: pdwActualStreamIndex,
			PdwStreamFlags:       flags,
			PllTimestamp:         pllTimestamp,
			PpSample:             ppSample,
		}, nil
	}
}

// GetNextFrame issues an asynchronous read request. It fails once the device
// has been closed or the session has stopped itself after exhausting its
// empty-sample retry budget.
func (d *CaptureAsync) GetNextFrame() error {
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	if stopped {
		return utils.ErrorAsyncRetryExhausted
	}
	if d.Device.isClosed() {
		return utils.ErrorDeviceNotOpen
	}

	d.Device.mu.Lock()
	reader := d.Device.reader
	d.Device.mu.Unlock()
	if reader == nil {
		return utils.ErrorDeviceNotOpen
	}

	return reader.ReadSample(consts.MF_SOURCE_READER_FIRST_VIDEO_STREAM, 0, nil, nil, nil, nil)
}

// handleReadSample contains the callback's frame-delivery policy and is kept
// free of syscall machinery so it can be unit tested directly. It is invoked
// once per OnReadSample callback.
//
// Delivery guarantees:
//   - After CloseDevice has started, no user callback ever receives a frame.
//   - An empty sample re-issues a read request only up to
//     maxEmptySampleRetries; afterwards the device error callback is notified
//     exactly once and the session stops re-requesting until capture restarts.
//   - A real sample resets the consecutive empty-sample counter.
func (a *CaptureAsync) handleReadSample(hrStatus uint32, dwStreamIndex uint32, dwStreamFlags uint32, llTimestamp int64, pSample *mf.IMFSample) uintptr {
	// Gate the whole callback: once closing begins this returns false and we
	// leave without touching any device-owned object.
	if !a.Device.beginCallback() {
		return consts.S_OK
	}
	defer a.Device.endCallback()

	if consts.HRESULT(hrStatus) != consts.S_OK {
		a.reportError(consts.HResultError{Code: consts.HRESULT(hrStatus)})
		return uintptr(hrStatus)
	}

	if pSample == nil {
		return a.onEmptySample()
	}
	if pSample.S == nil {
		return consts.S_OK
	}

	// A real sample arrived: the empty-sample streak is over.
	a.mu.Lock()
	a.emptySampleCount = 0
	fn := a.callback.OnReadSampleFunc
	a.mu.Unlock()

	if fn != nil {
		fn(a, dwStreamIndex, dwStreamFlags, llTimestamp, pSample)
	}
	return consts.S_OK
}

// onEmptySample handles a callback carrying no sample (stream tick). It
// re-requests the next frame while the retry budget allows it; once the budget
// is exhausted it reports a single error and stops.
func (a *CaptureAsync) onEmptySample() uintptr {
	a.mu.Lock()
	if a.stopped {
		// Already gave up for this session; stay quiet until a restart.
		a.mu.Unlock()
		return consts.S_OK
	}
	a.emptySampleCount++
	exhausted := a.emptySampleCount > maxEmptySampleRetries
	if exhausted {
		a.stopped = true
	}
	a.mu.Unlock()

	if exhausted {
		a.reportError(utils.ErrorAsyncRetryExhausted)
		return consts.S_OK
	}

	if err := a.GetNextFrame(); err != nil {
		a.reportError(err)
	}
	return consts.S_OK
}

// reportError forwards an error to the device error callback at most once per
// capture session, and never while the device is closing.
func (a *CaptureAsync) reportError(err error) {
	if a.Device.isClosed() {
		return
	}
	a.reportOnce.Do(func() {
		if a.Device.isClosed() {
			return
		}
		a.Device.SetError(err)
	})
}

type CaptureBuffer struct {
	Buffer         []byte
	Length         uint32
	Total          uint32
	internalBuffer *mf.IMFMediaBuffer
	interSample    *mf.IMFSample
}

func (c *CaptureBuffer) Release() {
	if c.internalBuffer != nil {
		_ = c.internalBuffer.Unlock()
		_ = c.internalBuffer.Release()
		c.internalBuffer = nil
	}

	c.Buffer = nil
	c.Length = 0
	c.Total = 0
}

func (d *Device) ParseSampleToBuffer(sample *mf.IMFSample) (buffer *CaptureBuffer, err error) {
	if sample == nil {
		return nil, utils.ErrorParameterInvalid
	}
	cBuffer, err := sample.ConvertToContiguousBuffer()
	if err != nil {
		return nil, err
	}

	goBuffer, totalLen, currentLen, err := cBuffer.Lock()
	if err != nil {
		return nil, err
	}

	return &CaptureBuffer{
		Buffer:         goBuffer[:totalLen],
		Length:         currentLen,
		Total:          totalLen,
		internalBuffer: cBuffer,
		interSample:    sample,
	}, nil
}

func (d *CaptureAsync) GetConfig() (*ConfigParameter, error) {
	return d.Device.getConfig()
}

func (d *CaptureSync) GetConfig() (*ConfigParameter, error) {
	return d.Device.getConfig()
}

// SetConfigWithCheck will call getConfig to diff the current config and the new config, then set the new config if they are different.
func (d *CaptureSync) SetConfigWithCheck(newConfig *ConfigParameter) error {
	return d.Device.setConfigWithCheck(newConfig)
}
func (d *CaptureAsync) SetConfigWithCheck(newConfig *ConfigParameter) error {
	return d.Device.setConfigWithCheck(newConfig)
}

func (d *Device) setConfigWithCheck(newConfig *ConfigParameter) error {
	if newConfig == nil {
		return utils.ErrorParameterInvalid
	}
	currentConfig, err := d.getConfig()
	if err != nil {
		return err
	}
	cameraCtl, procImp, err := d.GetControl()
	if err != nil {
		return err
	}
	defer cameraCtl.Release()
	defer procImp.Release()

	if newConfig.Pan != currentConfig.Pan {
		err = cameraCtl.Set(consts.CameraControl_Pan, newConfig.Pan.Current, consts.KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Tilt != currentConfig.Tilt {
		err = cameraCtl.Set(consts.CameraControl_Tilt, newConfig.Tilt.Current, consts.KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Roll != currentConfig.Roll {
		err = cameraCtl.Set(consts.CameraControl_Roll, newConfig.Roll.Current, consts.KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Zoom != currentConfig.Zoom {
		err = cameraCtl.Set(consts.CameraControl_Zoom, newConfig.Zoom.Current, consts.KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Exposure != currentConfig.Exposure {
		err = cameraCtl.Set(consts.CameraControl_Exposure, newConfig.Exposure.Current, consts.KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Iris != currentConfig.Iris {
		err = cameraCtl.Set(consts.CameraControl_Iris, newConfig.Iris.Current, consts.KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Focus != currentConfig.Focus {
		err = cameraCtl.Set(consts.CameraControl_Focus, newConfig.Focus.Current, consts.KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}

	if newConfig.Brightness != currentConfig.Brightness {
		err = procImp.Set(consts.VideoProcAmp_Brightness, newConfig.Brightness.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Contrast != currentConfig.Contrast {
		err = procImp.Set(consts.VideoProcAmp_Contrast, newConfig.Contrast.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Hue != currentConfig.Hue {
		err = procImp.Set(consts.VideoProcAmp_Hue, newConfig.Hue.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Saturation != currentConfig.Saturation {
		err = procImp.Set(consts.VideoProcAmp_Saturation, newConfig.Saturation.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Sharpness != currentConfig.Sharpness {
		err = procImp.Set(consts.VideoProcAmp_Sharpness, newConfig.Sharpness.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Gamma != currentConfig.Gamma {
		err = procImp.Set(consts.VideoProcAmp_Gamma, newConfig.Gamma.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.ColorEnable != currentConfig.ColorEnable {
		err = procImp.Set(consts.VideoProcAmp_ColorEnable, newConfig.ColorEnable.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.WhiteBalance != currentConfig.WhiteBalance {
		err = procImp.Set(consts.VideoProcAmp_WhiteBalance, newConfig.WhiteBalance.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.BacklightCompensation != currentConfig.BacklightCompensation {
		err = procImp.Set(consts.VideoProcAmp_BacklightCompensation, newConfig.BacklightCompensation.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	if newConfig.Gain != currentConfig.Gain {
		err = procImp.Set(consts.VideoProcAmp_Gain, newConfig.Gain.Current, consts.KSPROPERTY_VIDEOPROCAMP_FLAGS_MANUAL)
		if err != nil {
			return err
		}
	}
	return nil
}
