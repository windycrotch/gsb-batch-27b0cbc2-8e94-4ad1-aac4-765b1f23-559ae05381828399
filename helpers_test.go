// Copyright ©2022 Evolution. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Reusable helpers and fixtures for tests.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"testing"

	"github.com/evolution-gaming/ease/internal/tools"
)

// fixPlanConfig fixture provides simple encoding plan.
//
// To make "encoding" faster we just copy source to destination, for the
// purposes of tests it is irrelevant if we use realistic encoder or just a
// simple file copy.
func fixPlanConfig(t *testing.T) (fPath string) {
	payload := []byte(`{
		"Inputs": [
			"testdata/video/testsrc01.mp4"
		],
		"Schemes": [
			{
				"Name": "simple_src_duplication",
				"CommandTpl": ["cp -v ",  "%INPUT% ", "%OUTPUT%.mp4"]
			}
		]
	}`)
	fPath = path.Join(t.TempDir(), "minimal.json")
	err := os.WriteFile(fPath, payload, fs.FileMode(0o644))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	return
}

// fixPlanConfigInvalid fixture provides invalid encoding plan.
func fixPlanConfigInvalid(t *testing.T) (fPath string) {
	payload := []byte(`{
		"Inputs": [
			"non-existent"
		]
	}`)
	fPath = path.Join(t.TempDir(), "minimal.json")
	err := os.WriteFile(fPath, payload, fs.FileMode(0o644))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	return fPath
}

// fixCreateFakeFfmpegAndPutItOnPath fixture create a fake and failing ffmpeg on PATH.
func fixCreateFakeFfmpegAndPutItOnPath(t *testing.T) {
	origPath := os.Getenv("PATH")
	fakePath := t.TempDir()
	t.Setenv("PATH", fmt.Sprintf("%s:%s", fakePath, origPath))

	// Make ffmpeg binary contain this helper contents, will just print to
	// stderr all provided command line args. This should be enough to trigger
	// failure for anything that requires proper function from ffmpeg.
	src, err := os.Open("testdata/helpers/stderr")
	if err != nil {
		t.Fatalf("Unable to open source: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(path.Join(fakePath, "ffmpeg"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("Unable to open destination: %v", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatalf("Failure copying: %v", err)
	}
}

// fixPlanConfigMisalignedFrames fixture returns a plan which will result in encoded file
// shorter by 1 frame e.g. first frame dropped.
//
// Note: this plan assumes ffmpeg doing actual encoding!
func fixPlanConfigMisalignedFrames(t *testing.T) (fPath string) {
	ffmpegPath, err := tools.FfmpegPath()
	if err != nil {
		t.Fatalf("ffmpeg not found: %v", err)
	}
	payload := fmt.Appendf(nil, `{
		"Inputs": [
			"testdata/video/testsrc01.mp4"
		],
		"Schemes": [
			{
				"Name": "misaligned",
				"CommandTpl": ["%s -i %%INPUT%% -vf \"trim=start_frame=1\" %%OUTPUT%%.mp4"]
			}
		]
	}`, ffmpegPath)

	fPath = path.Join(t.TempDir(), "misaligned_plan.json")
	err = os.WriteFile(fPath, payload, fs.FileMode(0o644))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	return
}

// fixPlanConfigMixed fixture returns a plan with several inputs and schemes, where one
// encoding command is guaranteed to fail, while others succeed.
//
// "Inputs" are two copies of the same source (different names, since duplicate inputs
// are not allowed), so several independent plan entries are generated.
func fixPlanConfigMixed(t *testing.T) (fPath string) {
	ffmpegPath, err := tools.FfmpegPath()
	if err != nil {
		t.Fatalf("ffmpeg not found: %v", err)
	}

	// Copy source file so that plan has two distinct inputs.
	src2 := path.Join(t.TempDir(), "testsrc01_copy.mp4")
	copyFile(t, "testdata/video/testsrc01.mp4", src2)

	payload := fmt.Appendf(nil, `{
		"Inputs": [
			"testdata/video/testsrc01.mp4",
			"%s"
		],
		"Schemes": [
			{
				"Name": "good",
				"CommandTpl": ["%s -i %%INPUT%% -an -c:v copy -y %%OUTPUT%%.mp4"]
			},
			{
				"Name": "failing",
				"CommandTpl": ["exit 1 %%INPUT%% %%OUTPUT%%"]
			}
		]
	}`, src2, ffmpegPath)

	fPath = path.Join(t.TempDir(), "mixed_plan.json")
	err = os.WriteFile(fPath, payload, fs.FileMode(0o644))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	return
}

// fixPlanConfigAllFailing fixture returns a plan where all encoding commands fail.
func fixPlanConfigAllFailing(t *testing.T) (fPath string) {
	payload := []byte(`{
		"Inputs": [
			"testdata/video/testsrc01.mp4"
		],
		"Schemes": [
			{
				"Name": "failing1",
				"CommandTpl": ["exit 1 %INPUT% %OUTPUT%"]
			},
			{
				"Name": "failing2",
				"CommandTpl": ["exit 2 %INPUT% %OUTPUT%"]
			}
		]
	}`)
	fPath = path.Join(t.TempDir(), "all_failing_plan.json")
	err := os.WriteFile(fPath, payload, fs.FileMode(0o644))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	return
}

// fixPlanConfigOneVQMFailing fixture returns a plan with two independent entries, where
// one entry encodes successfully but will fail VQM measurement (frame count mismatch),
// while the other entry encodes and measures fine.
func fixPlanConfigOneVQMFailing(t *testing.T) (fPath string) {
	ffmpegPath, err := tools.FfmpegPath()
	if err != nil {
		t.Fatalf("ffmpeg not found: %v", err)
	}

	src2 := path.Join(t.TempDir(), "testsrc01_copy.mp4")
	copyFile(t, "testdata/video/testsrc01.mp4", src2)

	payload := fmt.Appendf(nil, `{
		"Inputs": [
			"testdata/video/testsrc01.mp4",
			"%s"
		],
		"Schemes": [
			{
				"Name": "good",
				"CommandTpl": ["%s -i %%INPUT%% -an -c:v copy -y %%OUTPUT%%.mp4"]
			},
			{
				"Name": "bad",
				"CommandTpl": ["%s -i %%INPUT%% -an -vf \"trim=start_frame=1\" -y %%OUTPUT%%.mp4"]
			}
		]
	}`, src2, ffmpegPath, ffmpegPath)

	fPath = path.Join(t.TempDir(), "one_vqm_failing_plan.json")
	err = os.WriteFile(fPath, payload, fs.FileMode(0o644))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	return
}

// copyFile is a helper to copy file contents.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("Unable to read source file %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, fs.FileMode(0o644)); err != nil {
		t.Fatalf("Unable to write destination file %s: %v", dst, err)
	}
}

// fakeFfmpegScript is a fake ffmpeg implementation sufficient to drive the "run"
// command without real libvmaf support.
//
// It succeeds for the VMAF support probe and writes a valid libvmaf JSON report when
// invoked for VQM measurement (detected by "log_path=" in filter arguments).
const fakeFfmpegScript = `#!/bin/sh
log=""
for a in "$@"; do
	case "$a" in
		*log_path=*)
			rest="${a#*log_path=}"
			log="${rest%%:*}"
			;;
	esac
done
if [ -n "$log" ]; then
	cat > "$log" <<'JSON'
{"frames":[
{"frameNum":0,"metrics":{"vmaf":90.0,"psnr":40.0,"float_ms_ssim":0.980}},
{"frameNum":1,"metrics":{"vmaf":91.0,"psnr":41.0,"float_ms_ssim":0.981}},
{"frameNum":2,"metrics":{"vmaf":92.0,"psnr":42.0,"float_ms_ssim":0.982}},
{"frameNum":3,"metrics":{"vmaf":93.0,"psnr":43.0,"float_ms_ssim":0.983}},
{"frameNum":4,"metrics":{"vmaf":94.0,"psnr":44.0,"float_ms_ssim":0.984}}
]}
JSON
fi
exit 0
`

// fakeFfprobeScript is a fake ffprobe implementation.
//
// It emits metadata or per-packet stats depending on requested entries. A compressed
// file whose name contains "_bad" is reported with a different frame count than the
// source, which makes VQM measurement fail with a frame count mismatch.
const fakeFfprobeScript = `#!/bin/sh
file=""
mode="meta"
for a in "$@"; do
	file="$a"
	case "$a" in
		*packet=*) mode="packets" ;;
	esac
done

frames=5
case "$file" in
	*_bad*) frames=4 ;;
esac

if [ "$mode" = "packets" ]; then
	printf '{"packets":['
i=0
while [ "$i" -lt "$frames" ]; do
	if [ "$i" -gt 0 ]; then printf ','; fi
	printf '{"flags":"__","pts_time":"0.%02d","duration_time":"0.04","size":"1000"}' "$i"
	i=$((i+1))
done
	printf ']}\n'
	exit 0
fi

printf '{"streams":[{"codec_name":"h264","r_frame_rate":"25/1","duration":"0.2","width":320,"height":240,"bit_rate":"200000","nb_read_frames":"%d"}],"format":{"duration":"0.2","bit_rate":"200000"}}\n' "$frames"
exit 0
`

// fixCreateFakeFfmpegAndFfprobeOnPath puts fake ffmpeg and ffprobe binaries first on
// PATH, allowing the real "run" command to be exercised deterministically without
// actual libvmaf support.
func fixCreateFakeFfmpegAndFfprobeOnPath(t *testing.T) {
	t.Helper()
	origPath := os.Getenv("PATH")
	fakePath := t.TempDir()
	t.Setenv("PATH", fmt.Sprintf("%s:%s", fakePath, origPath))

	for name, content := range map[string]string{
		"ffmpeg":  fakeFfmpegScript,
		"ffprobe": fakeFfprobeScript,
	} {
		p := path.Join(fakePath, name)
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatalf("Unable to write fake %s: %v", name, err)
		}
	}
}

// fixPlanConfigWithFakeVQM returns a plan where compressed file names contain either
// "_good" or "_bad", which drives the fake ffprobe VQM outcome.
func fixPlanConfigWithFakeVQM(t *testing.T) (fPath string) {
	payload := []byte(`{
		"Inputs": [
			"testdata/video/testsrc01.mp4"
		],
		"Schemes": [
			{
				"Name": "good",
				"CommandTpl": ["cp -v ", "%INPUT% ", "%OUTPUT%.mp4"]
			},
			{
				"Name": "bad",
				"CommandTpl": ["cp -v ", "%INPUT% ", "%OUTPUT%.mp4"]
			}
		]
	}`)
	fPath = path.Join(t.TempDir(), "fake_vqm_plan.json")
	err := os.WriteFile(fPath, payload, fs.FileMode(0o644))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	return
}
