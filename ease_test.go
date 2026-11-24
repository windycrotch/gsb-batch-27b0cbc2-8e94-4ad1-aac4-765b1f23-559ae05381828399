// Copyright ©2022 Evolution. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// Tests for ease tool subcommands.
package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/evolution-gaming/ease/internal/encoding"
	"github.com/evolution-gaming/ease/internal/it"
	"github.com/evolution-gaming/ease/internal/testutil"
)

// Happy path functional test for run sub-command.
func Test_RunApp_Run(t *testing.T) {
	testutil.EnsureFfmpegWithVMAF(t)
	tempDir := t.TempDir()
	ePlan := fixPlanConfig(t)
	outDir := path.Join(tempDir, "out")

	t.Run("Should succeed execution with -plan flag", func(t *testing.T) {
		// Run command will generate encoding artifacts and analysis artifacts.
		app := CreateRunCommand()
		err := app.Run([]string{"-plan", ePlan, "-out-dir", outDir})
		it.Should(t, err == nil, "Unexpected error running encode: got = %v, want = nil", err)
	})

	t.Run("Should have a CSV report file", func(t *testing.T) {
		fd, err2 := os.Open(path.Join(outDir, "report.csv"))
		it.Must(t, err2 == nil, "Unexpected error opening report.csv: got = %v, want = nil", err2)
		defer fd.Close()
		records, err3 := csv.NewReader(fd).ReadAll()
		it.Must(t, err3 == nil, "Unexpected error reading CSV records: got = %v, want = nil", err3)
		// Expect 2 records: CSV header + record for 1 encoding.
		wantCount := 2
		gotCount := len(records)
		it.Should(t, gotCount == wantCount, "Unexpected number of records in report file: got = %v, want = %v", gotCount, wantCount)
	})

	t.Run("Should create plots", func(t *testing.T) {
		plotFileGlobs := []string{
			fmt.Sprintf("%s/*/*bitrate.png", outDir),
			fmt.Sprintf("%s/*/*vmaf.png", outDir),
			fmt.Sprintf("%s/*/*psnr.png", outDir),
		}

		for _, g := range plotFileGlobs {
			plots, _ := filepath.Glob(g)
			it.Must(t, plots != nil, "Glob match should not be nil")
			got := len(plots)
			it.Should(t, got == 1, "Expecting one file matching glob \"%v\": got = %v", g, got)
		}
	})
}

// Repeated execution of the same plan into the same output directory should be
// rejected, existing report must not be overwritten.
func Test_RunApp_Run_RepeatIntoSameOutDirShouldBeRejected(t *testing.T) {
	testutil.EnsureFfmpegWithVMAF(t)
	plan := fixPlanConfig(t)
	outDir := path.Join(t.TempDir(), "out")

	// First run should succeed and produce a report.
	err := CreateRunCommand().Run([]string{"-plan", plan, "-out-dir", outDir})
	it.Must(t, err == nil, "First run should succeed: got = %v", err)

	reportPath := path.Join(outDir, "report.csv")
	origReport, err := os.ReadFile(reportPath)
	it.Must(t, err == nil, "Unexpected error reading report: %v", err)

	// Second run into the same out dir must be rejected.
	gotErr := CreateRunCommand().Run([]string{"-plan", plan, "-out-dir", outDir})
	it.Must(t, gotErr != nil, "Second run into non-empty out dir should fail")

	var appErr *AppError
	it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)
	it.Should(t, appErr.ExitCode() != 0, "Exit code should be non-zero: got = %v", appErr.ExitCode())
	it.Should(t, strings.Contains(gotErr.Error(), "non-empty out dir"), "got = %v, want to contain %v", gotErr, "non-empty out dir")

	// Existing report should be left intact.
	gotReport, err := os.ReadFile(reportPath)
	it.Must(t, err == nil, "Unexpected error reading report: %v", err)
	it.Should(t, string(gotReport) == string(origReport), "Existing report should not be overwritten")
}

/*************************************
* Negative tests for run sub-command.
 *************************************/

// Error cases for run sub-command flags.
func Test_RunApp_Run_FlagErrors(t *testing.T) {
	// For some cases we need existing plan config file.
	planConfig := fixPlanConfig(t)

	tempDir := t.TempDir()

	tests := map[string]struct {
		// substring in Error()
		want      string
		givenArgs []string
	}{
		"Wrong flags": {
			givenArgs: []string{"-zzz", "aaaa", "-plan", planConfig, "-out-dir", path.Join(tempDir, "out1")},
			want:      "run usage error",
		},
		"Mandatory plan flag missing": {
			givenArgs: []string{"-out-dir", path.Join(tempDir, "out2")},
			want:      "mandatory option -plan is missing",
		},
		"Mandatory out-dir flag missing": {
			givenArgs: []string{"-plan", planConfig},
			want:      "mandatory option -out-dir is missing",
		},
		"Non-existent plan": {
			givenArgs: []string{"-plan", "a/yyy", "-out-dir", path.Join(tempDir, "out3")},
			want:      "encoding plan file does not exist?",
		},
		"Non-existent config file": {
			givenArgs: []string{"-conf", "missing-conf.json", "-plan", planConfig, "-out-dir", path.Join(tempDir, "out4v")},
			want:      "no such file or directory",
		},
		"Empty flags": {
			givenArgs: []string{},
			want:      "mandatory option",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := CreateRunCommand()
			// Discard usage output so that during test execution test output is
			// not flooded with command Usage/Help stuff.
			cmd.fs.SetOutput(io.Discard)
			gotErr := cmd.Run(tc.givenArgs)
			it.Must(t, gotErr != nil, "Should have error")
			it.Should(t, strings.Contains(gotErr.Error(), tc.want), "got = %v, want to contain %v", gotErr, tc.want)
		})
	}
}

func Test_RunApp_Run_WithFailedVQM(t *testing.T) {
	// Create a fake ffmpeg and modify PATH so that it's picked up first and
	// blows up VQM calculation.
	fixCreateFakeFfmpegAndPutItOnPath(t)

	app := CreateRunCommand()
	plan := fixPlanConfig(t)
	outDir := path.Join(t.TempDir(), "out")

	wantErrMsg := "FFmpeg VMAF support validation"
	wantExitCode := 1
	gotErr := app.Run([]string{"-plan", plan, "-out-dir", outDir})
	it.Must(t, gotErr != nil, "Should have error")
	it.Should(t, strings.Contains(gotErr.Error(), wantErrMsg), "got = %v, want to contain %v", gotErr, wantErrMsg)

	var appErr *AppError
	it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)

	gotExitCode := appErr.ExitCode()
	it.Should(t, gotExitCode == wantExitCode, "Exit code mismatch: got = %v, want = %v", gotExitCode, wantExitCode)
}

func Test_RunApp_Run_WithInvalidPlanConfigParseError(t *testing.T) {
	app := CreateRunCommand()
	wantErrMsg := "PlanConfig not valid: validation error with reasons"
	wantExitCode := 1

	gotErr := app.Run([]string{"-plan", fixPlanConfigInvalid(t), "-out-dir", t.TempDir()})
	it.Must(t, gotErr != nil, "Should have error")
	it.Should(t, strings.Contains(gotErr.Error(), wantErrMsg), "got = %v, want to contain %v", gotErr, wantErrMsg)

	var appErr *AppError
	it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)

	gotExitCode := appErr.ExitCode()
	it.Should(t, gotExitCode == wantExitCode, "Exit code mismatch: got = %v, want = %v", gotExitCode, wantExitCode)
}

func Test_RunApp_Run_WithNonEmptyOutDirShouldTerminate(t *testing.T) {
	app := CreateRunCommand()
	plan := fixPlanConfig(t)
	// Dir containing plan file by definition is non-empty.
	outDir := path.Dir(plan)

	t.Logf("Given existing out dir: %s", outDir)
	err := os.MkdirAll(outDir, 0o755)
	it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)

	t.Log("When plan is executed")
	gotErr := app.Run([]string{"-plan", plan, "-out-dir", outDir})

	t.Log("Then there is an error and program terminates")
	wantErrMsg := "non-empty out dir"
	it.Must(t, gotErr != nil, "Should have error")
	it.Should(t, strings.Contains(gotErr.Error(), wantErrMsg), "got = %v, want to contain %v", gotErr, wantErrMsg)

	var appErr *AppError
	it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)

	wantExitCode := 1
	gotExitCode := appErr.ExitCode()
	it.Should(t, gotExitCode == wantExitCode, "Exit code mismatch: got = %v, want = %v", gotExitCode, wantExitCode)
}

func Test_RunApp_Run_WithInvalidApplicationConfig(t *testing.T) {
	invalidConfig := []byte(`{}`)
	confFile := path.Join(t.TempDir(), "wrong.json")
	// Empty configuration is wrong configuration. When we explicitly specify
	// configuration file, we expect all options to be defined.
	err := os.WriteFile(confFile, invalidConfig, 0o600)
	it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)

	app := CreateRunCommand()
	gotErr := app.Run([]string{"-plan", fixPlanConfigInvalid(t), "-out-dir", t.TempDir(), "-conf", confFile})

	var expErr *AppError
	it.Must(t, errors.As(gotErr, &expErr), "Expecting error of type AppError: got = %v", gotErr)
}

func Test_RunApp_Run_MisalignedFrames(t *testing.T) {
	plan := fixPlanConfigMisalignedFrames(t)
	app := CreateRunCommand()
	gotErr := app.Run([]string{"-plan", plan, "-out-dir", t.TempDir()})

	var expErr *AppError
	it.Must(t, errors.As(gotErr, &expErr), "Expecting error of type AppError: got = %v", gotErr)
	wantErrMsg := "VQM calculations had errors, see log for reasons"
	it.Should(t, strings.Contains(gotErr.Error(), wantErrMsg), "got = %v, want to contain %v", gotErr, wantErrMsg)
}

/*************************************
* Partial failure tests for run sub-command.
 *************************************/

// reportRow is a decoded CSV report row keyed by header name.
type reportRow map[string]string

// readReport reads CSV report from given output directory.
func readReport(t *testing.T, outDir string) (header []string, rows []reportRow) {
	t.Helper()
	fd, err := os.Open(path.Join(outDir, "report.csv"))
	it.Must(t, err == nil, "Unexpected error opening report.csv: got = %v, want = nil", err)
	defer fd.Close()

	records, err := csv.NewReader(fd).ReadAll()
	it.Must(t, err == nil, "Unexpected error reading CSV records: got = %v, want = nil", err)
	it.Must(t, len(records) > 0, "Report should have at least a header row")

	header = records[0]
	for _, r := range records[1:] {
		row := make(reportRow, len(header))
		for i, h := range header {
			row[h] = r[i]
		}
		rows = append(rows, row)
	}
	return header, rows
}

// plotDirs returns names of analysis/plot subdirectories under given output directory.
func plotDirs(t *testing.T, outDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	it.Must(t, err == nil, "Unexpected error reading out dir: %v", err)
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// assertNoPlots checks that no analysis/plot subdirectories were created in outDir.
func assertNoPlots(t *testing.T, outDir string) {
	t.Helper()
	gotDirs := plotDirs(t, outDir)
	it.Should(t, len(gotDirs) == 0, "No plot directories expected: got = %v", gotDirs)
}

// assertPlotsForEntries checks that plot directories exist exactly for the given
// successfully measured entries and that each such directory contains required plots.
func assertPlotsForEntries(t *testing.T, outDir string, okRows []reportRow) {
	t.Helper()
	wantDirs := make(map[string]struct{}, len(okRows))
	for _, r := range okRows {
		base := path.Base(r["CompressedFile"])
		base = strings.TrimSuffix(base, path.Ext(base))
		wantDirs[base] = struct{}{}

		// These plots are always produced by the default VMAF template.
		for _, suffix := range []string{"_bitrate.png", "_vmaf.png", "_psnr.png"} {
			p := path.Join(outDir, base, base+suffix)
			it.Should(t, testutil.FileExists(p), "Missing plot file %s for successful entry", p)
		}
	}

	gotDirs := plotDirs(t, outDir)
	it.Must(t, len(gotDirs) == len(wantDirs), "Unexpected plot directories: got = %v, want = %v", gotDirs, wantDirs)
	for _, d := range gotDirs {
		_, ok := wantDirs[d]
		it.Should(t, ok, "Unexpected plot directory for failed entry: %s", d)
	}
}

// When some encodings fail, remaining entries should still be processed and reported,
// report should include all planned entries and command should exit with non-zero code.
func Test_RunApp_Run_PartialEncodeFailure(t *testing.T) {
	testutil.EnsureFfmpegWithVMAF(t)
	plan := fixPlanConfigMixed(t)
	outDir := path.Join(t.TempDir(), "out")

	app := CreateRunCommand()
	gotErr := app.Run([]string{"-plan", plan, "-out-dir", outDir})

	t.Run("Should exit with non-zero code", func(t *testing.T) {
		var appErr *AppError
		it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)
		it.Should(t, appErr.ExitCode() != 0, "Exit code should be non-zero: got = %v", appErr.ExitCode())
	})

	t.Run("Report should be written and parseable", func(t *testing.T) {
		// Report should be present despite the failure.
		it.Should(t, testutil.FileExists(path.Join(outDir, "report.csv")), "report.csv should exist")
	})

	_, rows := readReport(t, outDir)
	// 2 inputs * 2 schemes = 4 planned entries.
	wantCount := 4
	it.Must(t, len(rows) == wantCount, "Report should contain all planned entries: got = %v, want = %v", len(rows), wantCount)

	var okRows, failedRows []reportRow
	for _, r := range rows {
		if r["Status"] == "ok" {
			okRows = append(okRows, r)
		} else {
			failedRows = append(failedRows, r)
		}
	}

	t.Run("Should have both successful and failed entries", func(t *testing.T) {
		it.Must(t, len(okRows) == 2, "Expecting 2 successful entries: got = %v", len(okRows))
		it.Must(t, len(failedRows) == 2, "Expecting 2 failed entries: got = %v", len(failedRows))
	})

	t.Run("Failed entries should retain command and source file info", func(t *testing.T) {
		for _, r := range failedRows {
			it.Should(t, r["Status"] == "encoding_failed", "Unexpected status: got = %v, want = %v", r["Status"], "encoding_failed")
			it.Should(t, r["Cmd"] != "", "Failed entry should retain Cmd: got = %q, want non-empty", r["Cmd"])
			it.Should(t, r["SourceFile"] != "", "Failed entry should retain SourceFile: got = %q, want non-empty", r["SourceFile"])
			it.Should(t, strings.Contains(r["Cmd"], "exit"), "Failed entry Cmd should be retained verbatim: got = %q", r["Cmd"])
		}
	})

	t.Run("Failed entries should have empty/zero metrics", func(t *testing.T) {
		metricFields := []string{
			"VMAFMean", "VMAFMin", "VMAFMax", "VMAFHarmonicMean", "VMAFStDev", "VMAFVariance",
			"PSNRMean", "PSNRMin", "PSNRMax", "PSNRHarmonicMean", "PSNRStDev", "PSNRVariance",
			"MS_SSIMMean", "MS_SSIMMin", "MS_SSIMMax", "MS_SSIMHarmonicMean", "MS_SSIMStDev", "MS_SSIMVariance",
			"BitrateMean", "BitrateMin", "BitrateMax",
			"VQMResultFile",
		}
		for _, r := range failedRows {
			for _, f := range metricFields {
				got := r[f]
				it.Should(t, got == "" || got == "0", "Metric field %q of failed entry should be empty or zero: got = %q", f, got)
			}
		}
	})

	t.Run("Successful entries should have full metrics", func(t *testing.T) {
		for _, r := range okRows {
			it.Should(t, r["VMAFMean"] != "" && r["VMAFMean"] != "0", "VMAFMean should be measured: got = %q", r["VMAFMean"])
			it.Should(t, r["PSNRMean"] != "" && r["PSNRMean"] != "0", "PSNRMean should be measured: got = %q", r["PSNRMean"])
			it.Should(t, r["BitrateMean"] != "" && r["BitrateMean"] != "0", "BitrateMean should be measured: got = %q", r["BitrateMean"])
			it.Should(t, r["VQMResultFile"] != "", "VQMResultFile should be set for successful entry: got = %q", r["VQMResultFile"])
		}
	})

	t.Run("Plots should exist only for successful entries", func(t *testing.T) {
		assertPlotsForEntries(t, outDir, okRows)
	})
}

// When all encodings fail, command should exit with non-zero code, but report must still
// be written and parseable.
func Test_RunApp_Run_AllEncodeFailed(t *testing.T) {
	plan := fixPlanConfigAllFailing(t)
	outDir := path.Join(t.TempDir(), "out")

	app := CreateRunCommand()
	gotErr := app.Run([]string{"-plan", plan, "-out-dir", outDir})

	var appErr *AppError
	it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)
	it.Should(t, appErr.ExitCode() != 0, "Exit code should be non-zero: got = %v", appErr.ExitCode())

	it.Should(t, testutil.FileExists(path.Join(outDir, "report.csv")), "report.csv should exist even if all entries failed")

	_, rows := readReport(t, outDir)
	it.Must(t, len(rows) == 2, "Report should contain all planned entries: got = %v, want = %v", len(rows), 2)
	for _, r := range rows {
		it.Should(t, r["Status"] == "encoding_failed", "Unexpected status: got = %v, want = %v", r["Status"], "encoding_failed")
		it.Should(t, r["Cmd"] != "", "Failed entry should retain Cmd: got = %q, want non-empty", r["Cmd"])
		it.Should(t, r["SourceFile"] != "", "Failed entry should retain SourceFile: got = %q, want non-empty", r["SourceFile"])
	}

	// No plots for failed entries.
	assertNoPlots(t, outDir)
}

// When VQM measurement fails for a single entry, that entry should be marked as failed,
// while other entries should still be measured, plotted and reported.
func Test_RunApp_Run_PartialVQMFailure(t *testing.T) {
	testutil.EnsureFfmpegWithVMAF(t)
	plan := fixPlanConfigOneVQMFailing(t)
	outDir := path.Join(t.TempDir(), "out")

	app := CreateRunCommand()
	gotErr := app.Run([]string{"-plan", plan, "-out-dir", outDir})

	var appErr *AppError
	it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)
	it.Should(t, appErr.ExitCode() != 0, "Exit code should be non-zero: got = %v", appErr.ExitCode())
	wantErrMsg := "VQM calculations had errors, see log for reasons"
	it.Should(t, strings.Contains(gotErr.Error(), wantErrMsg), "got = %v, want to contain %v", gotErr, wantErrMsg)

	it.Should(t, testutil.FileExists(path.Join(outDir, "report.csv")), "report.csv should exist")

	_, rows := readReport(t, outDir)
	// 2 inputs * 2 schemes = 4 planned entries.
	it.Must(t, len(rows) == 4, "Report should contain all planned entries: got = %v, want = %v", len(rows), 4)

	var ok, failed []reportRow
	for _, r := range rows {
		switch r["Status"] {
		case "vqm_failed":
			failed = append(failed, r)
		case "ok":
			ok = append(ok, r)
		default:
			t.Errorf("Unexpected status: got = %v", r["Status"])
		}
	}
	it.Should(t, len(failed) == 2, "Expecting 2 VQM-failed entries: got = %v", len(failed))
	it.Should(t, len(ok) == 2, "Expecting 2 successful entries: got = %v", len(ok))

	for _, r := range failed {
		it.Should(t, r["VMAFMean"] == "" || r["VMAFMean"] == "0", "VQM-failed entry should have no fabricated metrics: got = %q", r["VMAFMean"])
		it.Should(t, r["Cmd"] != "", "VQM-failed entry should retain Cmd: got = %q", r["Cmd"])
	}
	for _, r := range ok {
		it.Should(t, r["VMAFMean"] != "" && r["VMAFMean"] != "0", "Successful entry should have measured metrics: got = %q", r["VMAFMean"])
	}

	// Plots should exist only for successfully measured entries.
	assertPlotsForEntries(t, outDir, ok)
}

// Deterministic end-to-end test of partial VQM failure using fake ffmpeg/ffprobe
// binaries, thus not requiring real libvmaf support.
func Test_RunApp_Run_PartialVQMFailureWithFakeTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg/ffprobe are POSIX shell scripts")
	}
	fixCreateFakeFfmpegAndFfprobeOnPath(t)

	plan := fixPlanConfigWithFakeVQM(t)
	outDir := path.Join(t.TempDir(), "out")

	app := CreateRunCommand()
	gotErr := app.Run([]string{"-plan", plan, "-out-dir", outDir})

	var appErr *AppError
	it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)
	it.Should(t, appErr.ExitCode() != 0, "Exit code should be non-zero: got = %v", appErr.ExitCode())

	it.Should(t, testutil.FileExists(path.Join(outDir, "report.csv")), "report.csv should exist")

	_, rows := readReport(t, outDir)
	it.Must(t, len(rows) == 2, "Report should contain all planned entries: got = %v, want = %v", len(rows), 2)

	var ok, failed []reportRow
	for _, r := range rows {
		switch r["Status"] {
		case "vqm_failed":
			failed = append(failed, r)
		case "ok":
			ok = append(ok, r)
		default:
			t.Errorf("Unexpected status: got = %v", r["Status"])
		}
	}
	it.Should(t, len(failed) == 1, "Expecting 1 VQM-failed entry: got = %v", len(failed))
	it.Should(t, len(ok) == 1, "Expecting 1 successful entry: got = %v", len(ok))

	for _, r := range failed {
		it.Should(t, r["VMAFMean"] == "" || r["VMAFMean"] == "0", "VQM-failed entry should have no fabricated metrics: got = %q", r["VMAFMean"])
		it.Should(t, r["Cmd"] != "", "VQM-failed entry should retain Cmd: got = %q", r["Cmd"])
	}
	for _, r := range ok {
		// Fake ffmpeg emits VMAF values 90..94, mean is 92.
		it.Should(t, r["VMAFMean"] == "92", "Successful entry should have measured VMAF mean: got = %q, want = %q", r["VMAFMean"], "92")
	}

	assertPlotsForEntries(t, outDir, ok)
}

/*************************************
* Tests for bitrate sub-command.
 *************************************/

// Error cases for bitrate sub-command.
func Test_BitrateApp_Run_Errors(t *testing.T) {
	tempDir := t.TempDir()
	videoFile := "testdata/video/testsrc01.mp4"

	// Config file pointing to non-existent ffprobe, should fail config validation.
	invalidConf := path.Join(tempDir, "invalid.json")
	err := os.WriteFile(invalidConf, []byte(`{"ffprobe_path": "/non/existent/ffprobe"}`), 0o600)
	it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)

	tests := map[string]struct {
		// substring in Error()
		want         string
		givenArgs    []string
		wantExitCode int
	}{
		"Wrong flags": {
			givenArgs:    []string{"-zzz", "-i", videoFile},
			want:         "usage error",
			wantExitCode: 2,
		},
		"Non-existent config file": {
			givenArgs:    []string{"-conf", "missing-conf.json", "-i", videoFile},
			want:         "no such file or directory",
			wantExitCode: 1,
		},
		"Invalid config": {
			givenArgs:    []string{"-conf", invalidConf, "-i", videoFile},
			want:         "configuration validation",
			wantExitCode: 1,
		},
		"Mandatory input flag missing": {
			givenArgs:    []string{},
			want:         "mandatory option -i is missing",
			wantExitCode: 2,
		},
		"Non-existent input file": {
			givenArgs:    []string{"-i", "non-existent.mp4", "-o", path.Join(tempDir, "out.png")},
			want:         "video file should exist",
			wantExitCode: 1,
		},
		"Non-existent output dir": {
			givenArgs:    []string{"-i", videoFile, "-o", path.Join(tempDir, "no-such-dir", "out.png")},
			want:         "creating plot file",
			wantExitCode: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := CreateBitrateCommand()
			// Discard usage output so that test output is not flooded.
			cmd.fs.SetOutput(io.Discard)
			gotErr := cmd.Run(tc.givenArgs)
			it.Must(t, gotErr != nil, "Should have error")
			it.Should(t, strings.Contains(gotErr.Error(), tc.want), "got = %v, want to contain %v", gotErr, tc.want)

			var appErr *AppError
			it.Must(t, errors.As(gotErr, &appErr), "Expecting error of type AppError: got = %v", gotErr)
			gotExitCode := appErr.ExitCode()
			it.Should(t, gotExitCode == tc.wantExitCode, "Exit code mismatch: got = %v, want = %v", gotExitCode, tc.wantExitCode)
		})
	}
}

// When -o flag is omitted, plot PNG file name is derived from input file name
// and written to current working directory.
func Test_BitrateApp_Run_DefaultOutFile(t *testing.T) {
	wantFile := "testsrc01.png"
	videoFile, err := filepath.Abs("testdata/video/testsrc01.mp4")
	it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)

	// Change to temp dir in order to avoid polluting project directory.
	t.Chdir(t.TempDir())

	err = CreateBitrateCommand().Run([]string{"-i", videoFile})
	it.Must(t, err == nil, "Unexpected error running bitrate: got = %v, want = nil", err)

	it.Should(t, testutil.FileExists(wantFile), "bitrate plot file missing: %v", wantFile)
}

// Functional tests for other sub-commands..
func TestIntegration_AllSubcommands(t *testing.T) {
	testutil.EnsureFfmpegWithVMAF(t)
	tempDir := t.TempDir()
	outDir := path.Join(tempDir, "out")
	ePlan := fixPlanConfig(t)

	// Run command will generate encoding artifacts and analysis artifacts for later use
	// ans inputs.
	err := CreateRunCommand().Run([]string{"-plan", ePlan, "-out-dir", outDir})
	it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)

	t.Run("vqmplot should create plots", func(t *testing.T) {
		var vqmFile string
		// Need to get file with VQMs from encode stage.
		m, _ := filepath.Glob(fmt.Sprintf("%s/*vqm.json", outDir))
		wantMatches := 1
		gotMatches := len(m)
		it.Must(t, gotMatches == wantMatches, "got = %v, want = %v", gotMatches, wantMatches)
		vqmFile = m[0]

		for _, metric := range []string{"VMAF", "PSNR", "MS-SSIM"} {
			t.Run(metric, func(t *testing.T) {
				outFile := path.Join(tempDir, fmt.Sprintf("vqmplot_%s.png", metric))
				err := CreateVQMPlotCommand().Run([]string{"-i", vqmFile, "-o", outFile, "-m", metric, "-fps", "24"})
				it.Should(t, err == nil, "Unexpected error running vqmplot: got = %v, want = nil", err)
				it.Should(t, testutil.FileExists(outFile), "VQM file missing: %v", outFile)
			})
		}
	})

	t.Run("bitrate should create bitrate plot", func(t *testing.T) {
		var compressedFile string
		// Need to get compressed file from encode stage.
		m, _ := filepath.Glob(fmt.Sprintf("%s/*.mp4", outDir))
		wantMatches := 1
		gotMatches := len(m)
		it.Must(t, gotMatches == wantMatches, "got = %v, want = %v", gotMatches, wantMatches)
		compressedFile = m[0]

		outFile := path.Join(tempDir, "bitrate.png")
		err := CreateBitrateCommand().Run([]string{"-i", compressedFile, "-o", outFile})
		it.Should(t, err == nil, "Unexpected error running bitrate: got = %v, want = nil", err)
		it.Should(t, testutil.FileExists(outFile), "bitrate plot file missing: %v", outFile)
	})

	t.Run("new-plan should create plan template", func(t *testing.T) {
		planFile := path.Join(t.TempDir(), "plan.json")
		err := CreateNewPlanCommand().Run([]string{"-i", "video1.mp4", "-o", planFile})
		it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)

		b, err := os.ReadFile(planFile)
		it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)
		pc, err := encoding.NewPlanConfigFromJSON(b)
		it.Must(t, err == nil, "Unexpected error: got = %v, want = nil", err)

		it.Must(t, len(pc.Inputs) == 1, "got = %v, want = 1", len(pc.Inputs))
		it.Should(t, pc.Inputs[0] == "video1.mp4", "got = %v, want = %v", pc.Inputs[0], "video1.mp4")
		it.Should(t, len(pc.Schemes) > 0, "got = %v, want > 0", len(pc.Schemes))
	})
}
