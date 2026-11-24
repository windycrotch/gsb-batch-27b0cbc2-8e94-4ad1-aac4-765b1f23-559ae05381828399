package camera

import (
	"github.com/Kirizu-Official/windows-camera-go/utils"
	"github.com/Kirizu-Official/windows-camera-go/windows/guid"
	"github.com/Kirizu-Official/windows-camera-go/windows/mf"
)

// comSourceReaderProvider is the production sourceReaderProvider. It performs
// the real Media Foundation calls needed to build and configure a source
// reader. Tests replace it with an in-process fake instead of touching COM.
type comSourceReaderProvider struct {
	device *Device
}

func (p *comSourceReaderProvider) createSourceReader(config *CaptureConfig) (sourceReader, *mf.IMFMediaType, error) {
	d := p.device
	if d.pDeviceSource == nil || d.pDevPresent == nil {
		return nil, nil, utils.ErrorDeviceNotOpen
	}
	if config.enableAsync && config.callBack == 0 {
		return nil, nil, utils.ErrorAsyncNeedCallBack
	}

	mediaType, err := d.getSelectedMediaType(config.MediaType)
	if err != nil {
		return nil, nil, err
	}

	var sourceReaderAttributes *mf.IMFAttributes
	if err = mfPlat.MFCreateAttributes(&sourceReaderAttributes, 2); err != nil {
		_ = mediaType.Release()
		return nil, nil, err
	}

	if config.enableAsync {
		if err = sourceReaderAttributes.SetUnknown(guid.MF_SOURCE_READER_ASYNC_CALLBACK, config.callBack); err != nil {
			_ = sourceReaderAttributes.Release()
			_ = mediaType.Release()
			return nil, nil, err
		}
	}

	var reader *mf.IMFSourceReader
	if err = mfReadWrite.MFCreateSourceReaderFromMediaSource(d.pDeviceSource, sourceReaderAttributes, &reader); err != nil {
		_ = sourceReaderAttributes.Release()
		_ = mediaType.Release()
		return nil, nil, err
	}
	// The attributes object was only needed to construct the reader; the reader
	// keeps its own reference.
	_ = sourceReaderAttributes.Release()

	if err = reader.SetCurrentMediaType(0xFFFFFFFC, mediaType); err != nil {
		_ = reader.Release()
		_ = mediaType.Release()
		return nil, nil, err
	}

	return reader, mediaType, nil
}
