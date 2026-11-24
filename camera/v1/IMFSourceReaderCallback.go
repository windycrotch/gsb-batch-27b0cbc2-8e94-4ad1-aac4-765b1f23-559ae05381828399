package camera

import (
	"syscall"
	"unsafe"

	"github.com/Kirizu-Official/windows-camera-go/windows/consts"
	"github.com/Kirizu-Official/windows-camera-go/windows/guid"
	"github.com/Kirizu-Official/windows-camera-go/windows/mf"
)

var IID_IMFSourceReaderCallback = &guid.GUID{
	Data1: 0xdeec8d99,
	Data2: 0xfa1d,
	Data3: 0x4d82,
	Data4: [8]byte{0x84, 0xc2, 0x2c, 0x89, 0x69, 0x94, 0x48, 0x67},
}

type IMFSourceReaderCallbackVtbl struct {
	// IUnknown 方法
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr

	// IMFSourceReaderCallback 方法
	OnReadSample uintptr
	OnFlush      uintptr
	OnEvent      uintptr
}

type IMFSourceReaderCallback struct {
	Vtbl *IMFSourceReaderCallbackVtbl
}

type SourceReaderCallback struct {
	vtbl     IMFSourceReaderCallbackVtbl
	intf     IMFSourceReaderCallback
	refCount int32
	this     *CaptureAsync
	// 回调函数
	OnReadSampleFunc func(this *CaptureAsync, dwStreamIndex uint32, dwStreamFlags uint32, llTimestamp int64, pSample *mf.IMFSample)
	OnFlushFunc      func(this *CaptureAsync, dwStreamIndex uint32)
	OnEventFunc      func(this *CaptureAsync, dwStreamIndex uint32, pEvent *mf.IMFMediaEvent)
}

func newSourceReaderCallback(this *CaptureAsync) *SourceReaderCallback {
	callback := &SourceReaderCallback{
		this:     this,
		refCount: 1,
	}

	callback.vtbl = IMFSourceReaderCallbackVtbl{
		QueryInterface: syscall.NewCallback(callback.queryInterface),
		AddRef:         syscall.NewCallback(callback.addRef),
		Release:        syscall.NewCallback(callback.release),
		OnReadSample:   syscall.NewCallback(callback.onReadSample),
		OnFlush:        syscall.NewCallback(callback.onFlush),
		OnEvent:        syscall.NewCallback(callback.onEvent),
	}

	callback.intf = IMFSourceReaderCallback{
		Vtbl: &callback.vtbl,
	}

	return callback
}

func (c *SourceReaderCallback) GetInterface() *IMFSourceReaderCallback {
	return &c.intf
}

// SetOnReadSample set the callback function that will be called when a sample is read from the camera when use async.
//
// this: *camera.CaptureAsync is the async capture instance which from.
//
// dwStreamIndex: The zero-based index of the stream that delivered the sample.
//
// dwStreamFlags: A bitwise OR of zero or more flags from the consts.MF_SOURCE_READER_FLAG enumeration.
//
// llTimestamp: The time stamp of the sample, or the time of the stream event indicated in dwStreamFlags. The time is given in 100-nanosecond units.
//
// pSample: DO NOT RELEASE IT in callback! the sample that contains the buffer data, it will never be nil
//
// For more information, see https://learn.microsoft.com/en-us/windows/win32/api/mfreadwrite/nf-mfreadwrite-imfsourcereadercallback-onreadsample
func (c *SourceReaderCallback) SetOnReadSample(fn func(this *CaptureAsync, dwStreamIndex uint32, dwStreamFlags uint32, llTimestamp int64, pSample *mf.IMFSample)) {
	c.this.mu.Lock()
	c.OnReadSampleFunc = fn
	c.this.mu.Unlock()
}

func (c *SourceReaderCallback) SetOnFlush(fn func(this *CaptureAsync, dwStreamIndex uint32)) {
	c.this.mu.Lock()
	c.OnFlushFunc = fn
	c.this.mu.Unlock()
}

func (c *SourceReaderCallback) SetOnEvent(fn func(this *CaptureAsync, dwStreamIndex uint32, pEvent *mf.IMFMediaEvent)) {
	c.this.mu.Lock()
	c.OnEventFunc = fn
	c.this.mu.Unlock()
}

// IUnknown 方法实现
func (c *SourceReaderCallback) queryInterface(this uintptr, riidIn *syscall.GUID, ppvObject *uintptr) uintptr {
	if riidIn == nil || ppvObject == nil {
		return 0x80004003
	}
	riid := &guid.GUID{
		Data1: riidIn.Data1,
		Data2: riidIn.Data2,
		Data3: riidIn.Data3,
		Data4: riidIn.Data4,
	}

	*ppvObject = 0

	// 检查请求的接口
	if riid.IsMatch(IID_IMFSourceReaderCallback) {
		*ppvObject = this
		c.addRef(this)
		return 0
	}

	// IUnknown GUID
	iidUnknown := guid.GUID{
		Data1: 0x00000000,
		Data2: 0x0000,
		Data3: 0x0000,
		Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46},
	}

	if *riid == iidUnknown {
		*ppvObject = this
		c.addRef(this)
		return 0 // S_OK
	}

	return 0x80004002
}

func (c *SourceReaderCallback) addRef(this uintptr) uintptr {
	c.refCount++
	return uintptr(c.refCount)
}

func (c *SourceReaderCallback) release(this uintptr) uintptr {
	c.refCount--
	return uintptr(c.refCount)
}

// IMFSourceReaderCallback 方法实现
func (c *SourceReaderCallback) onReadSample(this uintptr, hrStatus uint32, dwStreamIndex uint32, dwStreamFlags uint32, llTimestamp int64, pSample uintptr) uintptr {
	var sample *mf.IMFSample
	if pSample != 0 {
		sample = (*mf.IMFSample)(unsafe.Pointer(pSample))
	}
	// All policy (closed gating, bounded empty-sample retry, once-only error
	// reporting, frame delivery) lives in CaptureAsync.handleReadSample so it
	// can be tested without COM.
	return c.this.handleReadSample(hrStatus, dwStreamIndex, dwStreamFlags, llTimestamp, sample)
}

func (c *SourceReaderCallback) onFlush(this uintptr, dwStreamIndex uint32) uintptr {
	// Never run user code once closing has started.
	if !c.this.Device.beginCallback() {
		return consts.S_OK
	}
	defer c.this.Device.endCallback()
	c.this.mu.Lock()
	fn := c.OnFlushFunc
	c.this.mu.Unlock()
	if fn != nil {
		fn(c.this, dwStreamIndex)
	}
	return consts.S_OK
}

func (c *SourceReaderCallback) onEvent(this uintptr, dwStreamIndex uint32, pEvent uintptr) uintptr {
	// Gate the whole callback, including the event Release, on the close
	// lifecycle so a late event cannot outlive the reader.
	if !c.this.Device.beginCallback() {
		return consts.S_OK
	}
	defer c.this.Device.endCallback()

	c.this.mu.Lock()
	fn := c.OnEventFunc
	c.this.mu.Unlock()
	if fn != nil && pEvent != 0 {
		event := (*mf.IMFMediaEvent)(unsafe.Pointer(pEvent))
		fn(c.this, dwStreamIndex, event)
		_ = event.Release()
	}
	return consts.S_OK
}

func (c *SourceReaderCallback) ToUintptr() uintptr {
	return uintptr(unsafe.Pointer(c.GetInterface()))
}
