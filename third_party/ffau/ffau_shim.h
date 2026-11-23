#ifndef FFAU_SHIM_H
#define FFAU_SHIM_H

/* Compatibility helpers so the Go bindings can build against FFmpeg 5+
 * (avcodec send/receive API, AVChannelLayout, packet/frame allocators). */

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/channel_layout.h>
#include <libavutil/samplefmt.h>
#include <libswresample/swresample.h>

static inline AVChannelLayout ffau_default_channel_layout(int n_channels) {
	AVChannelLayout layout;
	av_channel_layout_default(&layout, n_channels);
	return layout;
}

/* av_channel_layout_to_mask() was removed in FFmpeg 7; reconstruct the mask
 * from the layout's per-channel ids. */
static inline uint64_t ffau_channel_layout_to_mask(const AVChannelLayout *layout) {
	uint64_t mask = 0;
	if (layout->order == AV_CHANNEL_ORDER_NATIVE) {
		return layout->u.mask;
	}
	for (unsigned i = 0; i < layout->nb_channels; i++) {
#if LIBAVUTIL_VERSION_MAJOR >= 58
		enum AVChannel id = layout->u.map[i].id;
#else
		enum AVChannel id = layout->u.map[i];
#endif
		if (id > 0 && id < 64) {
			mask |= (uint64_t)1 << (id - 1);
		}
	}
	return mask;
}

static inline int ffau_averror(int e) {
	return AVERROR(e);
}

static inline int ffau_averror_eof(void) {
	return AVERROR_EOF;
}

static inline int ffau_averror_eagain(void) {
	return AVERROR(EAGAIN);
}

static inline int ffau_layout_nb_channels(AVChannelLayout layout) {
	return layout.nb_channels;
}

/* Resampler allocation using the swr_alloc_set_opts2 API.
 * Layouts are described by the legacy uint64 channel masks which are
 * converted to AVChannelLayout internally. */
static inline int ffau_swr_alloc(SwrContext **swr,
		uint64_t out_layout, enum AVSampleFormat out_fmt, int out_rate,
		uint64_t in_layout, enum AVSampleFormat in_fmt, int in_rate) {
	AVChannelLayout out_ch, in_ch;
	int ret;
	if ((ret = av_channel_layout_from_mask(&out_ch, out_layout)) < 0) {
		return ret;
	}
	if ((ret = av_channel_layout_from_mask(&in_ch, in_layout)) < 0) {
		return ret;
	}
	ret = swr_alloc_set_opts2(swr, &out_ch, out_fmt, out_rate,
			&in_ch, in_fmt, in_rate, 0, NULL);
	return ret;
}

#endif
