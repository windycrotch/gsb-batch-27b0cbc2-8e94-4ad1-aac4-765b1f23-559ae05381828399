package encoder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ffmpegCmd is the ffmpeg executable. It is a variable so tests can point it at
// a fake binary and exercise the real process handling code.
var ffmpegCmd = "ffmpeg"

// pipeDrainTimeout bounds how long Run waits for the encoder output pipe to
// close after the process exited. A process that inherited the pipe (for
// example a shell wrapper on Windows) must never keep a job from converging.
const pipeDrainTimeout = 2 * time.Second

// ErrCancelled is returned by Run when the encoder was stopped because the job
// was cancelled or its execution was superseded.
var ErrCancelled = errors.New("cancelled")

// SetFFmpegCommand overrides the ffmpeg executable and returns a function that
// restores the previous value. It is intended for tests.
func SetFFmpegCommand(cmd string) func() {
	previous := ffmpegCmd
	ffmpegCmd = cmd
	return func() { ffmpegCmd = previous }
}

// FFmpeg struct.
type FFmpeg struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	progress progress
	cancel   bool
	finished bool
}

type progress struct {
	Frame      int
	FPS        float64
	Bitrate    float64
	TotalSize  int
	OutTimeMS  int
	OutTime    string
	DupFrames  int
	DropFrames int
	Speed      string
	Progress   float64
}

// Snapshot is a point in time copy of the encoder progress. It is safe to read
// from another goroutine while the encoder is running.
type Snapshot struct {
	Frame      int
	FPS        float64
	Bitrate    float64
	TotalSize  int
	OutTimeMS  int
	OutTime    string
	DupFrames  int
	DropFrames int
	Speed      string
	Progress   float64
}

// ffmpegOptions struct passed into Ffmpeg.Run.
type ffmpegOptions struct {
	Input  string
	Output string

	Format formatOptions `json:"format"`
	Video  videoOptions  `json:"video"`
	Audio  audioOptions  `json:"audio"`
	Filter filterOptions `json:"filter"`

	Raw []string `json:"raw"` // Raw flag options.
}

type formatOptions struct {
	Container string `json:"container"`
	Clip      bool   `json:"clip"`
	StartTime string `json:"startTime"`
	StopTime  string `json:"stopTime"`
}

type videoOptions struct {
	Codec        string `json:"codec"`
	Preset       string `json:"preset"`
	Pass         string `json:"pass"`
	Crf          int    `json:"crf"`
	Bitrate      string `json:"bitrate"`
	MinRate      string `json:"minrate"`
	MaxRate      string `json:"maxrate"`
	BufSize      string `json:"bufsize"`
	PixelFormat  string `json:"pixel_format"`
	FrameRate    string `json:"frame_rate"`
	Speed        string `json:"speed"`
	Tune         string `json:"tune"`
	Profile      string `json:"profile"`
	Level        string `json:"level"`
	FastStart    bool   `json:"faststart"`
	Size         string `json:"size"`
	Width        string `json:"width"`
	Height       string `json:"height"`
	Format       string `json:"format"`
	Aspect       string `json:"aspect"`
	Scaling      string `json:"scaling"`
	CodecOptions string `json:"codec_options"`
}

type audioOptions struct {
	Codec      string `json:"codec"`
	Channel    string `json:"channel"`
	Quality    string `json:"quality"`
	SampleRate string `json:"sample_rate"`
	Volume     string `json:"volume"`
}

type filterOptions struct {
	Deband      bool   `json:"deband"`
	Deshake     bool   `json:"deshake"`
	Deflicker   bool   `json:"deflicker"`
	Dejudder    bool   `json:"dejudder"`
	Denoise     string `json:"denoise"`
	Deinterlace string `json:"deinterlace"`
	Brightness  string `json:"brightness"`
	Contrast    string `json:"contrast"`
	Saturation  string `json:"saturation"`
	Gamma       string `json:"gamma"`
	Acontrast   string `json:"acontrast"`
}

// Run runs the ffmpeg encoder with options.
//
// The context controls the lifetime of the encoder process: cancelling it kills
// the running ffmpeg process immediately instead of waiting for the next
// progress poll. Run is safe to call from one goroutine while Cancel is called
// from another.
func (f *FFmpeg) Run(ctx context.Context, input, output, data string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// Parse options and build the command.
	args, err := parseOptions(input, output, data)
	if err != nil {
		return err
	}

	log.Info("running FFmpeg with options: ", args)
	cmd := exec.CommandContext(ctx, ffmpegCmd, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	// Capture stderr (if any) in a temporary file. Using a file instead of an
	// io.Writer keeps exec from starting a copy goroutine, so Wait can not block
	// on a descendant process that inherited the pipe.
	stderrFile, err := ioutil.TempFile("", "openencoder-ffmpeg-*.log")
	if err != nil {
		return err
	}
	stderrPath := stderrFile.Name()
	defer func() {
		stderrFile.Close()
		os.Remove(stderrPath)
	}()
	cmd.Stderr = stderrFile

	// A Cancel that arrives before Run starts still has to win.
	f.mu.Lock()
	if f.finished {
		f.mu.Unlock()
		return ErrCancelled
	}
	alreadyCancelled := f.cancel
	f.cmd = cmd
	f.mu.Unlock()
	if alreadyCancelled {
		return ErrCancelled
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// A Cancel that raced with Start could not see the process yet; kill it now.
	if f.wasCancelled() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return ErrCancelled
	}

	// Drain stdout and record the latest progress until the pipe closes. This
	// must run concurrently, otherwise a cancelled process could block while
	// writing to a pipe nobody reads.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		f.updateProgress(stdout)
	}()

	waitErr := cmd.Wait()

	// Unblock the progress reader in case a descendant process still holds the
	// write end of the pipe; the job must converge either way.
	_ = stdout.Close()
	select {
	case <-drainDone:
	case <-time.After(pipeDrainTimeout):
		log.Warn("ffmpeg output pipe is still open; continuing")
	}

	if f.wasCancelled() || ctx.Err() != nil {
		return ErrCancelled
	}
	if waitErr != nil {
		return fmt.Errorf("%w: %s", waitErr, lastLine(readTempFile(stderrFile)))
	}
	return nil
}

// Cancel stops a running FFmpeg job immediately.
func (f *FFmpeg) Cancel() {
	f.mu.Lock()
	if f.finished {
		f.mu.Unlock()
		return
	}
	f.cancel = true
	cmd := f.cmd
	f.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	log.Warn("killing ffmpeg process")
	if err := cmd.Process.Kill(); err != nil {
		// Killing an already exited process is not an error for our purposes.
		log.Warn("failed to kill process: ", err)
	}
}

// Done marks the encoder as finished. Calls to Cancel after Done are no-ops, so
// a late cancel can never affect a job that already finished.
func (f *FFmpeg) Done() {
	f.mu.Lock()
	f.finished = true
	f.mu.Unlock()
}

// ProgressSnapshot returns a consistent copy of the encoder progress.
func (f *FFmpeg) ProgressSnapshot() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Snapshot{
		Frame:      f.progress.Frame,
		FPS:        f.progress.FPS,
		Bitrate:    f.progress.Bitrate,
		TotalSize:  f.progress.TotalSize,
		OutTimeMS:  f.progress.OutTimeMS,
		OutTime:    f.progress.OutTime,
		DupFrames:  f.progress.DupFrames,
		DropFrames: f.progress.DropFrames,
		Speed:      f.progress.Speed,
		Progress:   f.progress.Progress,
	}
}

func (f *FFmpeg) wasCancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancel
}

// Version gets the ffmpeg version.
func (f *FFmpeg) Version() string {
	out, _ := exec.Command(ffmpegCmd, "-version").Output()
	return string(out)
}

func (f *FFmpeg) updateProgress(stdout io.ReadCloser) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		str := strings.Replace(line, " ", "", -1)

		parts := strings.Split(str, " ")
		f.setProgressParts(parts)
	}
}

func (f *FFmpeg) setProgressParts(parts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := 0; i < len(parts); i++ {
		progressSplit := strings.SplitN(parts[i], "=", 2)
		if len(progressSplit) != 2 {
			continue
		}
		k := progressSplit[0]
		v := progressSplit[1]

		switch k {
		case "frame":
			frame, _ := strconv.Atoi(v)
			f.progress.Frame = frame
		case "fps":
			fps, _ := strconv.ParseFloat(v, 64)
			f.progress.FPS = fps
		case "bitrate":
			v = strings.Replace(v, "kbits/s", "", -1)
			bitrate, _ := strconv.ParseFloat(v, 64)
			f.progress.Bitrate = bitrate
		case "total_size":
			size, _ := strconv.Atoi(v)
			f.progress.TotalSize = size
		case "out_time_ms":
			outTimeMS, _ := strconv.Atoi(v)
			f.progress.OutTimeMS = outTimeMS
		case "out_time":
			f.progress.OutTime = v
		case "dup_frames":
			frames, _ := strconv.Atoi(v)
			f.progress.DupFrames = frames
		case "drop_frames":
			frames, _ := strconv.Atoi(v)
			f.progress.DropFrames = frames
		case "speed":
			f.progress.Speed = v
		case "progress":
			progress, _ := strconv.ParseFloat(v, 64)
			f.progress.Progress = progress
		}
	}
}

// readTempFile returns the captured stderr of the encoder.
func readTempFile(f *os.File) string {
	if f == nil {
		return ""
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	b, err := ioutil.ReadAll(io.LimitReader(f, 64*1024))
	if err != nil {
		return ""
	}
	return string(b)
}

// lastLine returns the last non-empty line of s, truncated for error messages.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len(line) > 300 {
				line = line[:300]
			}
			return line
		}
	}
	return "ffmpeg failed"
}

// parseOptions translates the preset data into ffmpeg command line arguments.
func parseOptions(input, output, data string) ([]string, error) {
	args := []string{
		"-hide_banner",
		"-loglevel", "error", // Set loglevel to fail job on errors.
		"-progress", "pipe:1",
		"-i", input,
	}

	// Decode JSON get options list from data.
	options := &ffmpegOptions{}
	if err := json.Unmarshal([]byte(data), &options); err != nil {
		return nil, fmt.Errorf("invalid ffmpeg options: %w", err)
	}

	// If raw options provided, add the list of raw options from ffmpeg presets.
	if len(options.Raw) > 0 {
		for _, v := range options.Raw {
			args = append(args, strings.Split(v, " ")...)
		}
		args = append(args, output)
		return args, nil
	}

	// Set options from struct.
	args = append(args, transformOptions(options)...)

	// Set 2 pass output if option is set.
	if options.Video.Pass == "2" {
		args = append(args, set2Pass(&args)...)
	}

	// Add output arg last.
	args = append(args, output)
	return args, nil
}

func setFormatFlags(opt formatOptions) []string {
	args := []string{}

	if opt.StartTime != "" {
		arg := []string{"-ss", opt.StartTime}
		args = append(args, arg...)
	}

	if opt.StopTime != "" {
		arg := []string{"-to", opt.StopTime}
		args = append(args, arg...)
	}

	return args
}

func setVideoFlags(opt videoOptions) []string {
	args := []string{}

	// Video codec.
	if opt.Codec != "" {
		args = append(args, []string{"-c:v", opt.Codec}...)
	}

	// Video preset.
	if opt.Preset != "" && opt.Preset != "none" {
		args = append(args, []string{"-preset", opt.Preset}...)
	}

	// CRF.
	if opt.Crf != 0 && opt.Pass == "crf" {
		crf := strconv.Itoa(opt.Crf)
		args = append(args, []string{"-crf", crf}...)
	}

	// Faststart.
	if opt.FastStart {
		args = append(args, []string{"-movflags", "faststart"}...)
	}

	// Bitrate.
	if opt.Bitrate != "" && opt.Bitrate != "0" {
		args = append(args, []string{"-b:v", opt.Bitrate}...)
	}

	// Minrate.
	if opt.MinRate != "" && opt.MinRate != "0" {
		args = append(args, []string{"-minrate", opt.MinRate}...)
	}

	// Maxrate.
	if opt.MaxRate != "" && opt.MaxRate != "0" {
		args = append(args, []string{"-maxrate", opt.MaxRate}...)
	}

	// Buffer Size.
	if opt.BufSize != "" && opt.BufSize != "0" {
		args = append(args, []string{"-bufsize", opt.BufSize}...)
	}

	// Pixel Format.
	if opt.PixelFormat != "" && opt.PixelFormat != "auto" {
		args = append(args, []string{"-pix_fmt", opt.PixelFormat}...)
	}

	// Frame Rate.
	if opt.FrameRate != "" && opt.PixelFormat != "auto" {
		args = append(args, []string{"-r", opt.FrameRate}...)
	}

	// Tune.
	if opt.Tune != "" && opt.Tune != "none" {
		args = append(args, []string{"-tune", opt.Tune}...)
	}

	// Profile.
	if opt.Profile != "" && opt.Profile != "none" {
		args = append(args, []string{"-profile:v", opt.Profile}...)
	}

	// Level.
	if opt.Level != "" && opt.Level != "none" {
		args = append(args, []string{"-level", opt.Level}...)
	}

	// Codec params.
	if opt.CodecOptions != "" && (opt.Codec == "libx264" || opt.Codec == "libx265") {
		p := strings.Replace(opt.Codec, "lib", "", 1)
		args = append(args, []string{"-" + p + "-params", opt.CodecOptions}...)
	}

	return args
}

func setVideoFilters(vopt videoOptions, opt filterOptions) string {
	args := []string{}

	// Speed.
	if vopt.Speed != "" && vopt.Speed != "auto" {
		args = append(args, []string{"setpts=" + vopt.Speed}...)
	}

	// Scale.
	scaleFilters := []string{}
	if vopt.Size != "" && vopt.Size != "source" {
		var arg string
		if vopt.Size == "custom" {
			arg = "scale=" + vopt.Width + ":" + vopt.Height
		} else if vopt.Format == "widescreen" {
			arg = "scale=" + vopt.Size + ":-1"
		} else {
			arg = "scale=-1:" + vopt.Size
		}
		scaleFilters = append(scaleFilters, arg)
	}

	if vopt.Scaling != "" && vopt.Scaling != "auto" {
		arg := "flags=" + vopt.Scaling
		scaleFilters = append(scaleFilters, arg)
	}

	// Add scale filters to vf flags if provided.
	if len(scaleFilters) > 0 {
		scaleFiltersStr := strings.Join(scaleFilters, ":")
		args = append(args, scaleFiltersStr)
	}

	// More filters.
	if opt.Deband {
		args = append(args, "deband")
	}

	if opt.Deshake {
		args = append(args, "deshake")
	}

	if opt.Deflicker {
		args = append(args, "deflicker")
	}

	if opt.Dejudder {
		args = append(args, "dejudder")
	}

	if opt.Denoise != "none" {
		var arg string

		switch opt.Denoise {
		case "light":
			arg = "removegrain=22"
		case "medium":
			arg = "vaguedenoiser=threshold=3:method=soft:nsteps=5"
		case "heavy":
			arg = "vaguedenoiser=threshold=6:method=soft:nsteps=5"
		default:
			arg = "removegrain=0"
		}
		args = append(args, arg)
	}

	if opt.Deinterlace != "none" {
		var arg string

		switch opt.Deinterlace {
		case "frame":
			arg = "yadif=0:-1:0"
		case "field":
			arg = "yadif=1:-1:0"
		case "frame_nospatial":
			arg = "yadif=2:-1:0"
		case "field_nospatial":
			arg = "yadif=3:-1:0"
		}
		args = append(args, arg)
	}

	// EQ filters.
	eq := []string{}

	if opt.Contrast != "" && opt.Contrast != "1" {
		eq = append(eq, []string{"contrast=" + opt.Contrast}...)
	}

	if opt.Brightness != "" && opt.Brightness != "0" {
		eq = append(eq, []string{"brightness=" + opt.Brightness}...)
	}

	if opt.Saturation != "" && opt.Saturation != "0" {
		eq = append(eq, []string{"saturation=" + opt.Saturation}...)
	}

	if opt.Gamma != "" && opt.Gamma != "0" {
		eq = append(eq, []string{"gamma=" + opt.Gamma}...)
	}

	if len(eq) > 0 {
		eqStr := strings.Join(eq, ":")
		args = append(args, []string{"eq=" + eqStr}...)
	}

	argsStr := strings.Join(args, ",")
	return argsStr
}

func setAudioFlags(opt audioOptions) []string {
	args := []string{}

	// Audio codec.
	if opt.Codec != "" {
		args = append(args, []string{"-c:a", opt.Codec}...)
	}

	// Channel.
	if opt.Channel != "" && opt.Channel != "source" {
		args = append(args, []string{"-rematrix_maxval", "1.0", "-ac", opt.Channel}...)
	}

	// Bitrate.
	if opt.Quality != "" && opt.Quality != "auto" {
		args = append(args, []string{"-b:a", opt.Quality}...)
	}

	// Sample rate.
	if opt.SampleRate != "" && opt.SampleRate != "auto" {
		args = append(args, []string{"-ar", opt.SampleRate}...)
	}

	return args
}

func setAudioFilters(opt audioOptions, filter filterOptions) string {
	args := []string{}

	if opt.Volume != "" && opt.Volume != "100" {
		v, _ := strconv.ParseFloat(opt.Volume, 64)
		args = append(args, []string{"volume=" + fmt.Sprintf("%.2f", v/100)}...)
	}

	if filter.Acontrast != "" && filter.Acontrast != "33" {
		a, _ := strconv.ParseFloat(filter.Acontrast, 64)
		args = append(args, []string{"acontrast=" + fmt.Sprintf("%.2f", a/100)}...)
	}

	argsStr := strings.Join(args, ",")
	return argsStr
}

func set2Pass(args *[]string) []string {
	op := "NUL &&" // Windows.
	cpy := make([]string, len(*args))
	copy(cpy, *args)

	*args = append(*args, []string{"-pass 1", "-f null", op}...)
	cpy = append([]string{"ffmpeg"}, cpy...)
	cpy = append(cpy, []string{"-pass 2"}...)

	return cpy
}

// transformOptions converts the ffmpegOptions{} struct and converts into
// a slice of ffmpeg options to be passed to exec.Command arguments.
func transformOptions(opt *ffmpegOptions) []string {
	args := []string{}

	// Set format flags if clip options are set.
	if opt.Format.Clip {
		arg := setFormatFlags(opt.Format)
		args = append(args, arg...)
	}

	// Video flags.
	args = append(args, setVideoFlags(opt.Video)...)

	// Video Filters.
	vf := []string{"-vf", setVideoFilters(opt.Video, opt.Filter)}

	// Only push -vf flag if there are video filter arguments.
	if vf[1] != "" {
		args = append(args, vf...)
	}

	// Audio flags.
	args = append(args, setAudioFlags(opt.Audio)...)

	// Audio filters.
	af := []string{"-af", setAudioFilters(opt.Audio, opt.Filter)}

	// Only push -af flag if there are audio filter arguments.
	if af[1] != "" {
		args = append(args, af...)
	}

	extra := []string{
		"-y",
	}
	args = append(args, extra...)
	return args
}
