package ffau

import (
	"errors"
	"fmt"
	"io"
	"unsafe"
)

/*
#cgo pkg-config: libavformat libavcodec libavutil libswresample
#include <libavutil/samplefmt.h>
#include <libavformat/avformat.h>
#include "ffau_shim.h"
*/
import "C"

/* FormatContext represents a decoding context of eg. an open file. */
type FormatContext struct {
	ctx *C.AVFormatContext
}

type SampleStream interface {
	/* The format in which this stream's samples are stored. */
	Format() AudioFormat

	/* Releases any resources associated with the stream. */
	Close()

	/* returns raw audio data (1 pointer per plane) and number of samples (per plane), or an error */
	read_raw() (**C.uint8_t, C.int, error)
}

type AudioStream struct {
	ctx    *FormatContext
	idx    C.int
	stream *C.AVStream
	codec  *C.AVCodecContext
	fmt    AudioFormat

	pkt       *C.AVPacket
	frame     *C.AVFrame
	framesEOF bool // demuxer reached EOF, flush packets have been sent
}

func avError(errnum C.int, fn string) error {
	switch errnum {
	case C.ffau_averror_eof():
		return io.EOF
	}
	var buf [256]C.char
	cp := (*C.char)(unsafe.Pointer(&buf[0]))
	C.av_strerror(errnum, cp, C.size_t(len(buf)))
	return errors.New(fmt.Sprintf("%s: %s", fn, C.GoString(cp)))
}

/* Opens an audio file and returns a FormatContext which can be used to decode
the file. */
func OpenFile(filename string) (*FormatContext, error) {
	var ctx FormatContext
	cfile := C.CString(filename)
	defer C.free(unsafe.Pointer(cfile))
	r := C.avformat_open_input(&ctx.ctx, cfile, nil, nil)
	if r < 0 {
		return nil, avError(r, "avformat_open_input: "+filename)
	}
	return &ctx, nil
}

/* Closes a FormatContext, releasing associated resources. */
func (format *FormatContext) Close() {
	C.avformat_close_input(&format.ctx)
}

func (format *FormatContext) stream(index int) *C.AVStream {
	if index < 0 || C.uint(index) >= format.ctx.nb_streams {
		panic(fmt.Sprintf("stream index %d outside range [0, %d)", index, int(format.ctx.nb_streams)))
	}
	stream := ((*[1 << 10]*C.AVStream)(unsafe.Pointer(format.ctx.streams)))[index]
	return stream
}

func (format *FormatContext) findStreamInfo() error {
	r := C.avformat_find_stream_info(format.ctx, nil)
	if r < 0 {
		return avError(r, "avformat_find_stream_info")
	}
	return nil
}

/* Returns the "best" AudioStream found in the file. */
func (format *FormatContext) OpenAudioStream() (*AudioStream, error) {
	err := format.findStreamInfo()
	if err != nil {
		return nil, err
	}
	idx := C.av_find_best_stream(format.ctx, C.AVMEDIA_TYPE_AUDIO, -1, -1, nil, 0)
	if idx < 0 {
		return nil, avError(idx, "av_find_best_stream")
	}
	stream := format.stream(int(idx))
	decoder := C.avcodec_find_decoder(stream.codecpar.codec_id)
	if decoder == nil {
		return nil, errors.New("No decoder available")
	}
	codec := C.avcodec_alloc_context3(decoder)
	if codec == nil {
		return nil, errors.New("Couldn't allocate codec context")
	}
	if r := C.avcodec_parameters_to_context(codec, stream.codecpar); r < 0 {
		C.avcodec_free_context(&codec)
		return nil, avError(r, "avcodec_parameters_to_context")
	}
	if r := C.avcodec_open2(codec, decoder, nil); r < 0 {
		C.avcodec_free_context(&codec)
		return nil, avError(r, "avcodec_open2")
	}
	audio := &AudioStream{ctx: format, idx: idx, stream: stream, codec: codec}
	audio.frame = C.av_frame_alloc()
	if audio.frame == nil {
		audio.Close()
		return nil, errors.New("Couldn't allocate frame")
	}
	audio.pkt = C.av_packet_alloc()
	if audio.pkt == nil {
		audio.Close()
		return nil, errors.New("Couldn't allocate packet")
	}
	layout := ChannelLayout(C.ffau_channel_layout_to_mask(&codec.ch_layout))
	if layout == 0 {
		layout = DefaultLayout(int(codec.ch_layout.nb_channels))
	}
	audio.fmt = AudioFormat{int(codec.sample_rate), SampleFmt(codec.sample_fmt), layout}
	return audio, nil
}

func (audio *AudioStream) Close() {
	if audio.codec != nil {
		C.avcodec_free_context(&audio.codec)
	}
	if audio.frame != nil {
		C.av_frame_free(&audio.frame)
	}
	if audio.pkt != nil {
		C.av_packet_free(&audio.pkt)
	}
}

func (audio AudioStream) Format() AudioFormat {
	return audio.fmt
}

/* read_frame fetches the next packet belonging to this stream. Returns io.EOF
 * once the demuxer is exhausted. */
func (audio *AudioStream) read_frame() error {
	for {
		C.av_packet_unref(audio.pkt)
		r := C.av_read_frame(audio.ctx.ctx, audio.pkt)
		if r < 0 {
			return avError(r, "av_read_frame")
		}
		if audio.pkt.stream_index == audio.idx {
			return nil
		}
		/* skip packets belonging to other streams */
	}
}

/* decode pulls one decoded frame into audio.frame. Returns true if a frame is
 * available, false to indicate the caller should retry (more input needed or
 * EOF while draining). */
func (audio *AudioStream) decode() (bool, error) {
	for {
		r := C.avcodec_receive_frame(audio.codec, audio.frame)
		if r >= 0 {
			return true, nil
		}
		if r != C.ffau_averror_eagain() && r != C.ffau_averror_eof() {
			return false, avError(r, "avcodec_receive_frame")
		}
		if r == C.ffau_averror_eof() {
			return false, io.EOF
		}
		/* EAGAIN: feed another packet (or flush at EOF) */
		if audio.framesEOF {
			if sr := C.avcodec_send_packet(audio.codec, nil); sr < 0 && sr != C.ffau_averror_eof() {
				return false, avError(sr, "avcodec_send_packet(flush)")
			}
			continue
		}
		err := audio.read_frame()
		if err == io.EOF {
			audio.framesEOF = true
			if sr := C.avcodec_send_packet(audio.codec, nil); sr < 0 && sr != C.ffau_averror_eof() {
				return false, avError(sr, "avcodec_send_packet(flush)")
			}
			continue
		} else if err != nil {
			return false, err
		}
		if sr := C.avcodec_send_packet(audio.codec, audio.pkt); sr < 0 && sr != C.ffau_averror_eagain() {
			return false, avError(sr, "avcodec_send_packet")
		}
	}
}

/* note: can return data chunks of length zero. error will be io.EOF at end of stream */
func (audio *AudioStream) read_raw() (**C.uint8_t, C.int, error) {
	gotFrame, err := audio.decode()
	if err != nil {
		return nil, 0, err
	}
	if !gotFrame {
		return nil, 0, nil
	}
	return audio.frame.extended_data, audio.frame.nb_samples, nil
}
