// Copyright ©2022 Evolution. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

// ease tool's run subcommand implementation.

package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/evolution-gaming/ease/internal/analysis"
	"github.com/evolution-gaming/ease/internal/encoding"
	"github.com/evolution-gaming/ease/internal/logging"
	"github.com/evolution-gaming/ease/internal/metric"
	"github.com/evolution-gaming/ease/internal/tools"
	"github.com/evolution-gaming/ease/internal/vqm"
	"github.com/jszwec/csvutil"
	"gonum.org/v1/gonum/stat"
)

// CreateRunCommand will create instance of App.
func CreateRunCommand() *App {
	longHelp := `Subcommand "run" will execute encoding plan according to definition in file
provided as parameter to -plan flag and will calculate and report VQM
metrics. This flag is mandatory.

Examples:

  ease run -plan plan.json -out-dir path/to/output/dir`

	app := &App{
		fs:          flag.NewFlagSet("run", flag.ContinueOnError),
		gf:          globalFlags{},
		mStore:      metric.NewStore(),
		entryStates: make(map[metric.ID]*entryState),
	}
	app.gf.Register(app.fs)
	app.fs.StringVar(&app.flPlan, "plan", "", "Encoding plan configuration file")
	app.fs.StringVar(&app.flOutDir, "out-dir", "", "Output directory to store results")
	app.fs.BoolVar(&app.flDryRun, "dry-run", false, "Do not actually run, just do checks and validation")
	app.fs.Usage = func() {
		printSubCommandUsage(longHelp, app.fs)
	}

	return app
}

// App is subcommand application context that implements Commander interface.
type App struct {
	// Configuration object
	cfg *Config
	// FlagSet instance
	fs *flag.FlagSet
	// Optional configuration file
	flPlan string
	// Output directory for analysis results
	flOutDir string
	// Global flags
	gf globalFlags
	// Dry run mode flag
	flDryRun bool
	// Encoding and VQ metric store
	mStore *metric.Store
	// Plan entry IDs in plan execution order.
	entryIDs []metric.ID
	// Evaluation status of each plan entry, keyed by metric store ID.
	entryStates map[metric.ID]*entryState
}

// entryState tracks evaluation status of a single plan entry (encoding command).
//
// Failures of an individual plan entry should not prevent remaining entries from
// being encoded, measured, reported and plotted.
type entryState struct {
	// Encoding command itself failed or did not produce expected artifact.
	encodeFailed bool
	// VQM measurement failed for otherwise successfully encoded artifact.
	vqmFailed bool
	// Analysis/plotting failed for otherwise successfully measured artifact.
	analysisFailed bool
}

// init will do App state initialization.
func (a *App) init(args []string) error {
	if err := a.fs.Parse(args); err != nil {
		return &AppError{
			exitCode: 2,
			msg:      fmt.Sprintf("%s usage error", a.fs.Name()),
		}
	}

	if a.gf.Debug {
		logging.EnableDebugLogger()
	}

	// Encoding plan config file is mandatory.
	if a.flPlan == "" {
		a.fs.Usage()
		return &AppError{
			exitCode: 2,
			msg:      "mandatory option -plan is missing",
		}
	}

	// Output dir is mandatory.
	if a.flOutDir == "" {
		a.fs.Usage()
		return &AppError{
			exitCode: 2,
			msg:      "mandatory option -out-dir is missing",
		}
	}

	// Encoding plan config file should exist.
	if _, err := os.Stat(a.flPlan); err != nil {
		a.fs.Usage()
		return &AppError{
			exitCode: 2,
			msg:      fmt.Sprintf("encoding plan file does not exist? %s", err),
		}
	}

	// Do not write over existing output directory.
	if isNonEmptyDir(a.flOutDir) {
		return &AppError{exitCode: 1, msg: fmt.Sprintf("non-empty out dir: %s", a.flOutDir)}
	}

	// Load application configuration.
	c, err := LoadConfig(a.gf.ConfFile)
	if err != nil {
		return &AppError{exitCode: 1, msg: err.Error()}
	}
	a.cfg = &c

	// Reset per-entry evaluation state, running App instance may be reused.
	a.mStore = metric.NewStore()
	a.entryIDs = nil
	a.entryStates = make(map[metric.ID]*entryState)

	return nil
}

// entryIDsInOrder returns plan entry IDs in plan execution order.
//
// Falls back to metric store order if entries were not tracked during encoding stage.
func (a *App) entryIDsInOrder() []metric.ID {
	if len(a.entryIDs) == len(a.mStore.GetIDs()) {
		return a.entryIDs
	}
	return a.mStore.GetIDs()
}

// encode will run encoding stage of plan execution.
//
// Encoding failures of individual plan entries are recorded but are not fatal for the
// batch: remaining entries are still encoded, measured and later reported. Only an
// error which prevents the batch from starting at all is returned.
func (a *App) encode(plan encoding.Plan) error {
	result, err := plan.Run()
	// Make sure to log any errors from RunResults.
	if ur := unrollResultErrors(result.RunResults); ur != "" {
		logging.Infof("Run had following ERRORS:\n%s", ur)
	}
	// If there are no per-command results at all, then the batch did not start (e.g.
	// output directory could not be created), which is a fatal error.
	if err != nil && len(result.RunResults) == 0 {
		return fmt.Errorf("plan run: %w", err)
	}

	if a.entryStates == nil {
		a.entryStates = make(map[metric.ID]*entryState, len(result.RunResults))
	}

	// Store encoding related metrics into mStore. Every planned entry gets a record,
	// irrespective of whether its encoding command succeeded, so that the final
	// report always reflects the complete plan.
	for _, res := range result.RunResults {
		st := &entryState{}
		// A command can fail, or (less likely) report success without producing the
		// expected artifact. In both cases later stages must not attempt to read the
		// missing artifact.
		//
		// Note: we deliberately check the command exit code rather than RunResult.Errors,
		// because the latter can also contain non-fatal ancillary errors, e.g. failure
		// to query metadata of the produced file.
		if res.ExitCode() != 0 {
			st.encodeFailed = true
			logging.Infof("Encoding command failed (exit code %d) for %s", res.ExitCode(), res.CompressedFile)
		}
		if !fileExists(res.CompressedFile) {
			st.encodeFailed = true
			logging.Infof("Encoded artifact is missing for %s", res.CompressedFile)
		}

		id := a.mStore.Insert(metric.Record{
			Name:             res.Name,
			SourceFile:       res.SourceFile,
			CompressedFile:   res.CompressedFile,
			Cmd:              res.Cmd,
			HStime:           res.Stats.HStime,
			HUtime:           res.Stats.HUtime,
			HElapsed:         res.Stats.HElapsed,
			Stime:            res.Stats.Stime,
			Utime:            res.Stats.Utime,
			Elapsed:          res.Stats.Elapsed,
			MaxRss:           res.Stats.MaxRss,
			VideoDuration:    res.VideoDuration,
			AvgEncodingSpeed: res.AvgEncodingSpeed,
		})
		a.entryStates[id] = st
		a.entryIDs = append(a.entryIDs, id)
		logging.Debugf("Storing record (id=%v) with encoding metrics", id)
	}

	// Do VQM calculations for successfully encoded videos. Entries which failed to
	// encode are skipped, failures of an individual measurement do not stop the rest.
	for _, id := range a.entryIDsInOrder() {
		record, err := a.mStore.Get(id)
		if err != nil {
			logging.Infof("Error retrieving record from metric store: %s", err)
			continue
		}

		st := a.entryStates[id]
		if st == nil {
			st = &entryState{}
			a.entryStates[id] = st
		}
		if st.encodeFailed {
			logging.Infof("Skip VQM measurement for %s: encoding failed", record.CompressedFile)
			continue
		}

		if err := a.measureVQM(&record); err != nil {
			st.vqmFailed = true
			logging.Infof("Failed to calculate VQM for %s due to error: %s", record.CompressedFile, err)
			continue
		}

		if err := a.mStore.Update(id, record); err != nil {
			st.vqmFailed = true
			logging.Infof("Error updating record (id=%v) for %s: %s", id, record.CompressedFile, err)
			continue
		}
		logging.Debugf("Updating record (id=%v) with VQ metrics", id)
		logging.Infof("Done measuring VQMs for %s", record.CompressedFile)
	}

	return nil
}

// measureVQM measures VQ metrics and bitrate for a single encoded file and fills
// corresponding fields of the given record.
//
// Returns an error if any of the measurements failed; in that case record is left
// untouched so that no partial/fabricated metric values are reported.
func (a *App) measureVQM(record *metric.Record) error {
	// Derive result file path.
	resFile := strings.TrimSuffix(record.CompressedFile, filepath.Ext(record.CompressedFile)) + "_vqm.json"
	// Create VMAF tool configuration.
	vmafCfg := vqm.FfmpegVMAFConfig{
		FfmpegPath:         a.cfg.FfmpegPath.Value(),
		FfmpegVMAFTemplate: a.cfg.FfmpegVMAFTemplate.Value(),
		ResultFile:         resFile,
	}

	vqmTool, err := vqm.NewFfmpegVMAF(&vmafCfg, record.CompressedFile, record.SourceFile)
	if err != nil {
		return fmt.Errorf("initializing VQM tool: %w", err)
	}

	logging.Infof("Start measuring VQMs for %s", record.CompressedFile)
	if err := vqmTool.Measure(); err != nil {
		return fmt.Errorf("measuring VQM: %w", err)
	}

	res, err := vqmTool.GetMetrics()
	if err != nil {
		return fmt.Errorf("getting VQM metrics: %w", err)
	}
	vqmUsageStat := vqmTool.UsageStat()

	fs, err := analysis.GetFrameStats(record.CompressedFile, a.cfg.FfprobePath.Value())
	if err != nil {
		return fmt.Errorf("getting per-frame stats: %w", err)
	}
	if len(fs) == 0 {
		return errors.New("getting per-frame stats: no frame stats received")
	}

	brate, err := fs.ToBitrate()
	if err != nil {
		return fmt.Errorf("getting bitrate: %w", err)
	}

	// Update record with VQ metrics.
	record.VQMResultFile = resFile
	record.VQMStime = vqmUsageStat.Stime
	record.VQMUtime = vqmUsageStat.Utime
	record.VQMElapsed = vqmUsageStat.Elapsed

	record.PSNRMin = res.PSNR.Min
	record.PSNRMax = res.PSNR.Max
	record.PSNRMean = res.PSNR.Mean
	record.PSNRHarmonicMean = res.PSNR.HarmonicMean
	record.PSNRStDev = res.PSNR.StDev
	record.PSNRVariance = res.PSNR.Variance

	record.VMAFMin = res.VMAF.Min
	record.VMAFMax = res.VMAF.Max
	record.VMAFMean = res.VMAF.Mean
	record.VMAFHarmonicMean = res.VMAF.HarmonicMean
	record.VMAFStDev = res.VMAF.StDev
	record.VMAFVariance = res.VMAF.Variance

	record.MS_SSIMMin = res.MS_SSIM.Min
	record.MS_SSIMMax = res.MS_SSIM.Max
	record.MS_SSIMMean = res.MS_SSIM.Mean
	record.MS_SSIMHarmonicMean = res.MS_SSIM.HarmonicMean
	record.MS_SSIMStDev = res.MS_SSIM.StDev
	record.MS_SSIMVariance = res.MS_SSIM.Variance

	record.BitrateMax = slices.Max(brate)
	record.BitrateMin = slices.Min(brate)
	record.BitrateMean = stat.Mean(brate, nil)

	return nil
}

// analyse will run analysis stage of plan execution.
//
// Only entries which were successfully encoded and measured are analysed. Failure of
// an individual entry's analysis is logged and does not prevent analysis of the
// remaining entries.
func (a *App) analyse() error {
	failed := 0
	for _, id := range a.entryIDsInOrder() {
		v, err := a.mStore.Get(id)
		if err != nil {
			failed++
			logging.Infof("Error fetching record by id (%v): %s", id, err)
			continue
		}

		st := a.entryStates[id]
		if st == nil {
			st = &entryState{}
			a.entryStates[id] = st
		}
		if st.encodeFailed || st.vqmFailed {
			logging.Infof("Skip analysis for %s: entry failed during encoding/VQM stage", v.CompressedFile)
			continue
		}

		if err := a.analyseEntry(v); err != nil {
			st.analysisFailed = true
			failed++
			logging.Infof("Failed to analyse %s: %s", v.CompressedFile, err)
		}
	}

	if failed != 0 {
		return fmt.Errorf("analysis failed for %d plan entries, see log for reasons", failed)
	}
	return nil
}

// analyseEntry creates VQM and bitrate plots for a single successfully measured
// plan entry.
func (a *App) analyseEntry(v metric.Record) (err error) {
	// Create separate dir for results.
	base := path.Base(v.CompressedFile)
	base = strings.TrimSuffix(base, path.Ext(base))
	logging.Infof("Analysing %s", v.CompressedFile)
	resDir := path.Join(a.flOutDir, base)
	if err = os.MkdirAll(resDir, os.FileMode(0o755)); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	// Do not leave artifacts of a failed analysis behind, only successful entries
	// should have a plot directory.
	defer func() {
		if err == nil {
			return
		}
		if rmErr := os.RemoveAll(resDir); rmErr != nil {
			logging.Infof("Unable to clean up analysis dir %s: %s", resDir, rmErr)
		}
	}()

	bitratePlot := path.Join(resDir, base+"_bitrate.png")
	vmafPlot := path.Join(resDir, base+"_vmaf.png")
	psnrPlot := path.Join(resDir, base+"_psnr.png")
	msssimPlot := path.Join(resDir, base+"_ms-ssim.png")

	// Need to get metadata of encoded video.
	meta, err := tools.FfprobeExtractMetadata(v.CompressedFile)
	if err != nil {
		return fmt.Errorf("extracting metadata: %w", err)
	}
	fps, err := parseFraction(meta.FrameRate)
	if err != nil {
		return fmt.Errorf("parsing frame rate: %w", err)
	}

	jsonFd, err := os.Open(v.VQMResultFile)
	if err != nil {
		return fmt.Errorf("opening VQM file: %w", err)
	}

	var frameMetrics vqm.FrameMetrics
	err = frameMetrics.FromFfmpegVMAF(jsonFd)
	// Close jsonFd file descriptor at earliest convenience. Should avoid use of defer
	// in loop in this case.
	jsonFd.Close()
	if err != nil {
		return fmt.Errorf("failed converting to FrameMetrics: %w", err)
	}

	size := len(frameMetrics)
	vmafs := make(metricXYs, 0, size)
	psnrs := make(metricXYs, 0, size)
	msssims := make(metricXYs, 0, size)
	for _, v := range frameMetrics {
		// Calculate timestamp for given frame.
		ts := float64(v.FrameNum) / fps
		vmafs = append(vmafs, metricXY{X: ts, Y: v.VMAF})
		psnrs = append(psnrs, metricXY{X: ts, Y: v.PSNR})
		msssims = append(msssims, metricXY{X: ts, Y: v.MS_SSIM})
	}

	// Since frameMetrics coming from JSON can be absent, we check for this case, e.g.
	// if all metric values are 0 then most probable case is that metric was missing
	// from source JSON. This is due to how unmarshaling works in Go.
	yIsZero := func(x metricXY) bool { return x.Y == 0 }
	skipVMAF := all(vmafs, yIsZero)
	skipPSNR := all(psnrs, yIsZero)
	skipMSSSIM := all(msssims, yIsZero)

	if err := analysis.MultiPlotBitrate(v.CompressedFile, bitratePlot, a.cfg.FfprobePath.Value()); err != nil {
		return fmt.Errorf("creating bitrate plot: %w", err)
	}
	logging.Infof("Bitrate plot done: %s", bitratePlot)

	if skipVMAF {
		logging.Info("Skip VMAF multi-plot, metric missing")
	} else {
		if err := analysis.MultiPlotVqm(vmafs, "VMAF", base, vmafPlot); err != nil {
			return fmt.Errorf("creating VMAF multiplot: %w", err)
		}
		logging.Infof("VMAF multi-plot done: %s", vmafPlot)
	}

	if skipPSNR {
		logging.Info("Skip PSNR multi-plot, metric missing")
	} else {
		if err := analysis.MultiPlotVqm(psnrs, "PSNR", base, psnrPlot); err != nil {
			return fmt.Errorf("creating PSNR multiplot: %w", err)
		}
		logging.Infof("PSNR multi-plot done: %s", psnrPlot)
	}

	if skipMSSSIM {
		logging.Info("Skip MS-SSIM multi-plot, metric missing")
	} else {
		if err := analysis.MultiPlotVqm(msssims, "MS-SSIM", base, msssimPlot); err != nil {
			return fmt.Errorf("creating MS-SSIM multiplot: %w", err)
		}
		logging.Infof("MS-SSIM multi-plot done: %s", msssimPlot)
	}

	return nil
}

// Entry evaluation statuses as reported in CSV report.
const (
	statusOK             = "ok"
	statusEncodingFailed = "encoding_failed"
	statusVQMFailed      = "vqm_failed"
	statusAnalysisFailed = "analysis_failed"
)

// status returns report status for the given plan entry.
func (s *entryState) status() string {
	switch {
	case s == nil:
		return statusOK
	case s.encodeFailed:
		return statusEncodingFailed
	case s.vqmFailed:
		return statusVQMFailed
	case s.analysisFailed:
		return statusAnalysisFailed
	default:
		return statusOK
	}
}

// saveReport writes recorded metrics to report file.
//
// Report contains records for all planned entries, including failed ones. Failed
// entries retain command and source file information, while their metric fields are
// left as zero values (no fabricated measurements).
func (a *App) saveReport() error {
	ids := a.entryIDsInOrder()
	report := make([]metric.Record, 0, len(ids))
	for _, id := range ids {
		r, err := a.mStore.Get(id)
		if err != nil {
			return fmt.Errorf("getting record (id=%v) from metric store: %w", id, err)
		}
		r.Status = a.entryStates[id].status()
		report = append(report, r)
	}

	reportPath := path.Join(a.flOutDir, a.cfg.ReportFileName.Value())
	reportOut, err := os.Create(reportPath)
	if err != nil {
		return fmt.Errorf("creating CSV report file: %w", err)
	}
	defer reportOut.Close()

	w := csv.NewWriter(reportOut)
	if err := csvutil.NewEncoder(w).Encode(report); err != nil {
		return fmt.Errorf("writing CSV report: %w", err)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("writing CSV report: %w", err)
	}

	return nil
}

// failureSummary returns human readable summary of failed plan entries.
//
// Empty string is returned when all plan entries were successfully evaluated.
func (a *App) failureSummary() string {
	var total, encodeFailed, vqmFailed, analyseFailed int
	for _, id := range a.entryIDsInOrder() {
		total++
		st := a.entryStates[id]
		if st == nil {
			continue
		}
		switch {
		case st.encodeFailed:
			encodeFailed++
		case st.vqmFailed:
			vqmFailed++
		case st.analysisFailed:
			analyseFailed++
		}
	}

	failed := encodeFailed + vqmFailed + analyseFailed
	if failed == 0 {
		return ""
	}

	details := make([]string, 0, 3)
	if encodeFailed != 0 {
		details = append(details, fmt.Sprintf("%d encoding", encodeFailed))
	}
	if vqmFailed != 0 {
		details = append(details, fmt.Sprintf("%d VQM", vqmFailed))
	}
	if analyseFailed != 0 {
		details = append(details, fmt.Sprintf("%d analysis", analyseFailed))
	}

	msg := fmt.Sprintf(
		"%d of %d plan entries failed (%s)",
		failed, total, strings.Join(details, ", "),
	)
	if vqmFailed != 0 {
		msg += "; VQM calculations had errors, see log for reasons"
	} else {
		msg += ", see log for reasons"
	}

	return msg
}

// Run is main entry point into App execution.
func (a *App) Run(args []string) error {
	logging.Infof("ease version: %s", vInfo)
	if err := a.init(args); err != nil {
		return err
	}

	// Verify VMAF support for detected FFmpeg.
	if err := tools.CheckFfmpegVMAFSupport(a.cfg.FfmpegPath.Value()); err != nil {
		return &AppError{exitCode: 1, msg: fmt.Sprintf("FFmpeg VMAF support validation: %s", err)}
	}

	logging.Debugf("Application configuration: %+v", a.cfg)
	// Check if configuration is valid.
	if err := a.cfg.Verify(); err != nil {
		return &AppError{exitCode: 1, msg: fmt.Sprintf("configuration validation: %s", err)}
	}

	logging.Debugf("Encoding plan config file: %v", a.flPlan)

	pc, err := createPlanConfig(a.flPlan)
	if err != nil {
		return &AppError{exitCode: 1, msg: err.Error()}
	}

	// To avoid ambiguity, resolve output path to absolute representation.
	outDirPath, err := filepath.Abs(a.flOutDir)
	if err != nil {
		return &AppError{exitCode: 1, msg: err.Error()}
	}
	plan := encoding.NewPlan(pc, outDirPath)

	// Early return in "dry run" mode.
	if a.flDryRun {
		logging.Info("Dry run mode finished!")
		return nil
	}

	// Run encode stage. Failures of individual encoding commands are not fatal for the
	// whole batch: remaining entries are still measured, reported and plotted.
	if err = a.encode(plan); err != nil {
		return &AppError{exitCode: 1, msg: err.Error()}
	}

	// Save report as early as possible. Report includes all planned entries, including
	// failed ones, hence it must be written even if some or all encodings failed.
	if err = a.saveReport(); err != nil {
		return &AppError{exitCode: 1, msg: err.Error()}
	}

	// Run analysis stage. Only successfully encoded and measured entries are plotted.
	// Analysis failure of an individual entry does not stop analysis of the rest.
	analysisErr := a.analyse()
	if analysisErr != nil {
		logging.Infof("Analysis stage had errors: %s", analysisErr)
	}

	// Update report with final per-entry statuses (analysis stage included).
	if err = a.saveReport(); err != nil {
		return &AppError{exitCode: 1, msg: err.Error()}
	}

	// Report failed entries with a non-zero exit code.
	if summary := a.failureSummary(); summary != "" {
		return &AppError{exitCode: 1, msg: summary}
	}
	if analysisErr != nil {
		return &AppError{exitCode: 1, msg: analysisErr.Error()}
	}

	logging.Info("Done")
	return nil
}
