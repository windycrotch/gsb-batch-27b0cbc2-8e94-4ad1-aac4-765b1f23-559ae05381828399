package encoder

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
)

// ffprobeCmd is the ffprobe executable. It is a variable so tests can point it
// at a fake binary.
var ffprobeCmd = "ffprobe"

// SetFFprobeCommand overrides the ffprobe executable and returns a function that
// restores the previous value. It is intended for tests.
func SetFFprobeCommand(cmd string) func() {
	previous := ffprobeCmd
	ffprobeCmd = cmd
	return func() { ffprobeCmd = previous }
}

// FFProbe struct.
type FFProbe struct{}

// Run runs an FFProbe command. It returns an empty response when the probe fails
// and keeps the historical signature for callers that do not need the error.
func (f FFProbe) Run(input string) *FFProbeResponse {
	resp, err := RunFFProbe(context.Background(), input)
	if err != nil || resp == nil {
		return &FFProbeResponse{}
	}
	return resp
}

// RunFFProbe runs ffprobe and decodes its JSON output. The context kills the
// probe process when the job is cancelled, so a long probe does not outlive its
// execution.
func RunFFProbe(ctx context.Context, input string) (*FFProbeResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	args := []string{
		"-i", input,
		"-show_streams",
		"-print_format", "json",
		"-v", "quiet",
	}

	// Execute command.
	cmd := exec.CommandContext(ctx, ffprobeCmd, args...)
	log.Info("Running FFprobe...")
	stdout, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		// ffprobe writes a JSON document even when it exits non-zero for a
		// stream it could not fully read; only reject the output if it is not
		// JSON below.
		log.Error(err.Error())
	}

	dat := &FFProbeResponse{}
	if err := json.Unmarshal(stdout, &dat); err != nil {
		if len(stdout) == 0 {
			return nil, errors.New("ffprobe returned no output")
		}
		return nil, err
	}
	return dat, nil
}

// FFProbeResponse defines the response from ffprobe.
type FFProbeResponse struct {
	Streams []Stream `json:"streams"`
}

// Stream describes a single media stream reported by ffprobe.
type Stream struct {
	Index              int         `json:"index"`
	CodecName          string      `json:"codec_name"`
	CodecLongName      string      `json:"codec_long_name"`
	Profile            string      `json:"profile"`
	CodecType          string      `json:"codec_type"`
	CodecTimeBase      string      `json:"codec_time_base"`
	CodecTagString     string      `json:"codec_tag_string"`
	CodecTag           string      `json:"codec_tag"`
	Width              int         `json:"width"`
	Height             int         `json:"height"`
	CodedWidth         int         `json:"coded_width"`
	CodedHeight        int         `json:"coded_height"`
	HasBFrames         int         `json:"has_b_frames"`
	SampleAspectRatio  string      `json:"sample_aspect_ratio"`
	DisplayAspectRatio string      `json:"display_aspect_ratio"`
	PixFmt             string      `json:"pix_fmt"`
	Level              int         `json:"level"`
	ChromaLocation     string      `json:"chroma_location"`
	Refs               int         `json:"refs"`
	IsAVC              string      `json:"is_avc"`
	NalLengthSize      string      `json:"nal_length_size"`
	RFrameRate         string      `json:"r_frame_rate"`
	AvgFrameRate       string      `json:"avg_frame_rate"`
	TimeBase           string      `json:"time_base"`
	StartPts           int         `json:"start_pts"`
	StartTime          string      `json:"start_time"`
	DurationTS         int         `json:"duration_ts"`
	Duration           string      `json:"duration"`
	BitRate            string      `json:"bit_rate"`
	BitsPerRawSample   string      `json:"bits_per_raw_sample"`
	NbFrames           string      `json:"nb_frames"`
	Disposition        disposition `json:"disposition"`
	Tags               tags        `json:"tags"`
}

type disposition struct {
	Default         int `json:"default"`
	Dub             int `json:"dub"`
	Original        int `json:"original"`
	Comment         int `json:"comment"`
	Lyrics          int `json:"lyrics"`
	Karoake         int `json:"karaoke"`
	Forced          int `json:"forced"`
	HearingImpaired int `json:"hearing_impaired"`
	VisualImpaired  int `json:"visual_empaired"`
	CleanEffects    int `json:"clean_effects"`
	AttachedPic     int `json:"attached_pic"`
	TimedThumbnails int `json:"timed_thumbnails"`
}

type tags struct {
	Language    string `json:"language"`
	HandlerName string `json:"handler_name"`
}
