//go:build freebsd || (linux && (mips || mips64 || mips64le || mipsle || ppc64 || ppc64le))

package oss

// ioctl numbers from <sys/soundcard.h> on FreeBSD / Linux PowerPC (BSD encoding).
var wantIoctl = map[string]int32{
	"DSP_RESET":          536891392,
	"DSP_SYNC":           536891393,
	"DSP_SPEED":          -1073459198,
	"DSP_SETFMT":         -1073459195,
	"DSP_CHANNELS":       -1073459194,
	"DSP_POST":           536891400,
	"DSP_SETFRAGMENT":    -1073459190,
	"DSP_GETFMTS":        1074024459,
	"DSP_GETOSPACE":      1074810892,
	"DSP_GETISPACE":      1074810893,
	"DSP_NONBLOCK":       536891406,
	"DSP_GETCAPS":        1074024463,
	"DSP_SETTRIGGER":     -2147201008,
	"DSP_SETDUPLEX":      536891414,
	"DSP_GETODELAY":      1074024471,
	"MIXER_READ_DEVMASK": 1074023934,
	"MIXER_READ_VOLUME":  1074023680,
	"MIXER_WRITE_VOLUME": -1073459968,
}
