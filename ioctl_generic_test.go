//go:build linux && (386 || amd64 || arm || arm64 || loong64 || riscv64 || s390x)

package oss

// ioctl numbers from <sys/soundcard.h> on Linux (asm-generic encoding).
var wantIoctl = map[string]int32{
	"DSP_RESET":          20480,
	"DSP_SYNC":           20481,
	"DSP_SPEED":          -1073459198,
	"DSP_SETFMT":         -1073459195,
	"DSP_CHANNELS":       -1073459194,
	"DSP_POST":           20488,
	"DSP_SETFRAGMENT":    -1073459190,
	"DSP_GETFMTS":        -2147201013,
	"DSP_GETOSPACE":      -2146414580,
	"DSP_GETISPACE":      -2146414579,
	"DSP_NONBLOCK":       20494,
	"DSP_GETCAPS":        -2147201009,
	"DSP_SETTRIGGER":     1074024464,
	"DSP_SETDUPLEX":      20502,
	"DSP_GETODELAY":      -2147201001,
	"MIXER_READ_DEVMASK": -2147201538,
	"MIXER_READ_VOLUME":  -2147201792,
	"MIXER_WRITE_VOLUME": -1073459968,
}
