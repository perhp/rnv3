package capture

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/cadu"
	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/livelog"
	"github.com/perhp/rnv3/internal/predict"
	"github.com/perhp/rnv3/internal/store"
)

// PostProcessor turns SatDump's raw products in workDir into final imagery
// (implemented by process.Pipeline; InventoryProcessor is the test/fallback
// implementation).
type PostProcessor interface {
	// Process returns how many satellite images were produced for the pass.
	Process(ctx context.Context, p store.Pass, sat config.Satellite, workDir, fileBase string, daylight bool) (int, error)
	// UpdateAggregates rebuilds station-wide artifacts (sky map, daily
	// mosaics/timelapses). Called after the pass reached its terminal DB
	// state so the aggregates include it.
	UpdateAggregates(ctx context.Context, passStart time.Time)
}

// InventoryProcessor counts the PNGs SatDump produced without transforming
// them — drives the decoded/failed decision without the image pipeline.
type InventoryProcessor struct{}

func (InventoryProcessor) Process(_ context.Context, _ store.Pass, _ config.Satellite, workDir, _ string, _ bool) (int, error) {
	count := 0
	err := filepath.WalkDir(workDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".png") {
			count++
		}
		return nil
	})
	return count, err
}

func (InventoryProcessor) UpdateAggregates(context.Context, time.Time) {}

// watchdogSlack is added to the capture duration as the hard process
// deadline; RN2 gave Meteor 900s of decode slack and NOAA none — rnv3 applies
// it uniformly so a hung SatDump can never wedge the scheduler.
const watchdogSlack = 15 * time.Minute

// killGrace is how long SatDump gets between SIGTERM/SIGKILL... on the
// deadline (os/exec sends Kill; WaitDelay covers the pipe drain).
const killGrace = 60 * time.Second

// Runner executes passes with SatDump. Implements sched.CaptureRunner.
// The config is snapshotted once at the start of each pass, so a SIGHUP
// reload can never mix old and new settings within one capture.
type Runner struct {
	Prov      *config.Provider
	St        *store.Store
	Processor PostProcessor
	// Live receives the decoder output as it happens, for the panel's
	// terminal; nil disables.
	Live *livelog.Hub
	// Exec substitutes the process constructor in tests; nil means the real
	// satdump binary from Paths.SatdumpBinary.
	Exec func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// live publishes one line to the panel terminal (no-op without a hub).
func (r *Runner) live(line string) {
	if r.Live != nil {
		r.Live.Publish(line)
	}
}

// Run owns the pass from AOS to a terminal state. Errors are recorded on the
// pass row rather than returned — the scheduler has nothing useful to do with
// them beyond logging, and the state machine is the source of truth.
func (r *Runner) Run(ctx context.Context, p store.Pass, sat config.Satellite) {
	cfg := r.Prov.Get() // one immutable snapshot for the whole capture
	log := slog.With("pass_id", p.ID, "satellite", p.Satellite)

	duration := time.Until(time.Unix(p.EndTS, 0))
	if duration < 30*time.Second {
		r.fail(p.ID, "pass window already over at capture start (scheduler fired late?)")
		return
	}
	captureSeconds := int(duration.Seconds())

	claimed, err := r.St.ClaimPass(p.ID)
	if err != nil {
		log.Error("cannot mark pass capturing", "err", err)
		return
	}
	if !claimed {
		log.Info("pass is no longer scheduled (cancelled or replanned); not capturing")
		return
	}
	if r.Live != nil {
		r.Live.Reset(p.ID)
	}
	r.live(fmt.Sprintf("=== %s pass %d: capturing for %ds (max elevation %.0f°)",
		sat.Name, p.ID, captureSeconds, p.MaxElevation))

	fileBase := FileBase(sat.Name, time.Unix(p.StartTS, 0))
	workDir, inRAM, err := makeWorkDir(cfg, p.ID, sat)
	if err != nil {
		r.fail(p.ID, fmt.Sprintf("cannot create work dir: %v", err))
		return
	}
	log.Info("capture starting", "duration_s", captureSeconds, "work_dir", workDir, "ramfs", inRAM, "file_base", fileBase)

	args, err := BuildArgs(cfg, sat, workDir, captureSeconds, p.StartTS)
	if err != nil {
		r.fail(p.ID, err.Error())
		return
	}

	snr := &SNRStats{}
	runErr := r.runSatdump(ctx, cfg, workDir, args, snr, log)

	if err := r.St.SetPassState(p.ID, store.StateProcessing, ""); err != nil {
		log.Error("cannot mark pass processing", "err", err)
	}
	r.live("=== capture finished, processing images")

	// Day/night classification at AOS+90s, RN2 parity.
	sunEl := predict.SunElevation(cfg.Station.Latitude, cfg.Station.Longitude,
		time.Unix(p.StartTS+90, 0))
	daylight := sunEl > sat.SunMinElevation

	// Recording + frame stats + audio retention per satellite type.
	recording := r.handleRecording(cfg, p, sat, workDir, fileBase, log)

	images := 0
	if procN, err := r.Processor.Process(ctx, p, sat, workDir, fileBase, daylight); err != nil {
		log.Error("post-processing failed", "err", err)
	} else {
		images = procN
	}

	switch {
	case images > 0:
		maxSNR, avgSNR, ok := snr.Result()
		res := store.CaptureResult{FileBase: fileBase, Daylight: daylight, Gain: sat.Gain}
		if ok {
			res.MaxSNR, res.AvgSNR = &maxSNR, &avgSNR
		}
		if err := r.St.CompleteCapture(p.ID, res); err != nil {
			log.Error("cannot mark pass decoded", "err", err)
		}
		log.Info("pass decoded", "images", images, "daylight", daylight, "max_snr", maxSNR)
		r.live(fmt.Sprintf("=== pass decoded: %d images", images))
		r.cleanupWorkDir(workDir, true)
	case !recording:
		r.fail(p.ID, withExitInfo("no recording produced", runErr))
		r.cleanupWorkDir(workDir, false)
	default:
		r.fail(p.ID, withExitInfo("decoder produced no images", runErr))
		r.cleanupWorkDir(workDir, false)
	}

	// After the terminal state, so the sky map / daily artifacts include this
	// pass — decoded or failed.
	r.Processor.UpdateAggregates(ctx, time.Unix(p.StartTS, 0))
}

// runSatdump executes satdump with a hard deadline, streaming its combined
// output through the SNR parser into a per-pass log file.
func (r *Runner) runSatdump(ctx context.Context, cfg *config.Config, workDir string, args []string, snr *SNRStats, log *slog.Logger) error {
	deadline := time.Duration(0)
	for i, a := range args { // recover the timeout for the watchdog deadline
		if a == "--timeout" && i+1 < len(args) {
			if secs, err := time.ParseDuration(args[i+1] + "s"); err == nil {
				deadline = secs
			}
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, deadline+watchdogSlack)
	defer cancel()

	newCmd := r.Exec
	if newCmd == nil {
		newCmd = exec.CommandContext
	}
	cmd := newCmd(runCtx, cfg.Paths.SatdumpBinary, args...)
	cmd.Dir = workDir
	cmd.WaitDelay = killGrace

	logFile, err := os.Create(filepath.Join(workDir, "satdump.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	log.Info("satdump starting", "args", strings.Join(args, " "))
	r.live("$ " + filepath.Base(cfg.Paths.SatdumpBinary) + " " + strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start satdump: %w", err)
	}

	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(scanCRorLF) // SatDump redraws progress with bare CRs
	for sc.Scan() {
		line := CleanLine(sc.Text())
		if line == "" {
			continue
		}
		snr.Feed(line)
		fmt.Fprintln(logFile, line)
		r.live(line)
	}

	err = cmd.Wait()
	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("satdump exceeded watchdog deadline (%s) and was killed", (deadline + watchdogSlack).Round(time.Second))
	}
	return err
}

// handleRecording verifies the raw recording exists, computes Meteor frame
// stats, and applies audio retention. Returns whether a recording was found.
func (r *Runner) handleRecording(cfg *config.Config, p store.Pass, sat config.Satellite, workDir, fileBase string, log *slog.Logger) bool {
	switch sat.Type {
	case config.SatNOAAAPT:
		wav := filepath.Join(workDir, "noaa_apt.wav")
		if !fileNonEmpty(wav) {
			return false
		}
		if cfg.Retention.DeleteNOAAAudio {
			os.Remove(wav)
		} else if err := moveFile(wav, filepath.Join(cfg.Paths.AudioNOAA, fileBase+".wav")); err != nil {
			log.Error("cannot retain wav", "err", err)
		}
		pruneOldFiles(cfg.Paths.AudioNOAA, cfg.Retention.AudioOlderThanDays, log)
		return true

	case config.SatMeteorLRPT:
		matches, _ := filepath.Glob(filepath.Join(workDir, "*.cadu"))
		if len(matches) == 0 || !fileNonEmpty(matches[0]) {
			return false
		}
		caduPath := matches[0]
		if st, err := cadu.FromFile(caduPath); err != nil {
			log.Warn("cadu stats failed", "err", err)
		} else {
			if err := r.St.SetFrameStats(p.ID, st.Received, st.Expected, st.LossPct, st.LargestGap); err != nil {
				log.Error("cannot store frame stats", "err", err)
			}
			log.Info("frame stats", "received", st.Received, "expected", st.Expected,
				"loss_pct", fmt.Sprintf("%.1f", st.LossPct), "largest_gap", st.LargestGap)
		}
		if cfg.Retention.DeleteMeteorAudio {
			os.Remove(caduPath)
		} else if err := moveFile(caduPath, filepath.Join(cfg.Paths.AudioMeteor, fileBase+".cadu")); err != nil {
			log.Error("cannot retain cadu", "err", err)
		}
		pruneOldFiles(cfg.Paths.AudioMeteor, cfg.Retention.AudioOlderThanDays, log)
		return true
	}
	return false
}

// makeWorkDir picks ramfs when enough memory is free (per-type threshold,
// RN2 parity), disk otherwise, and creates the per-pass directory.
func makeWorkDir(cfg *config.Config, passID int64, sat config.Satellite) (string, bool, error) {
	threshold := cfg.Capture.NOAAMemoryThresholdMB
	if sat.Type == config.SatMeteorLRPT {
		threshold = cfg.Capture.MeteorMemoryThresholdMB
	}
	base := cfg.Paths.Work
	inRAM := false
	if free := availableMemoryMB(); free >= threshold && threshold > 0 {
		if st, err := os.Stat(cfg.Paths.Ramfs); err == nil && st.IsDir() {
			base = filepath.Join(cfg.Paths.Ramfs, "work")
			inRAM = true
		}
	}
	dir := filepath.Join(base, fmt.Sprintf("pass-%d", passID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	return dir, inRAM, nil
}

// cleanupWorkDir removes the work dir after success; on failure it is kept
// for debugging (satdump.log lives there) — retention pruning or the next
// deploy cleans it up.
func (r *Runner) cleanupWorkDir(dir string, success bool) {
	if success {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("cannot remove work dir", "dir", dir, "err", err)
		}
	} else {
		slog.Info("keeping work dir for debugging", "dir", dir)
	}
}

func (r *Runner) fail(id int64, reason string) {
	slog.Error("pass failed", "pass_id", id, "reason", reason)
	r.live("=== pass failed: " + reason)
	if err := r.St.SetPassState(id, store.StateFailed, reason); err != nil {
		slog.Error("cannot mark pass failed", "pass_id", id, "err", err)
	}
}

// FileBase builds the image/audio filename base, identical to RN2's
// "<name-with-dashes>-<UTC yyyymmdd-hhmmss>" so migrated history and new
// captures share one naming scheme.
func FileBase(satName string, start time.Time) string {
	return strings.ReplaceAll(satName, " ", "-") + "-" + start.UTC().Format("20060102-150405")
}

func withExitInfo(reason string, runErr error) string {
	if runErr == nil {
		return reason
	}
	return fmt.Sprintf("%s (satdump: %v)", reason, runErr)
}

func fileNonEmpty(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// moveFile renames, falling back to copy+delete for cross-device moves
// (ramfs → disk is always cross-device).
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// pruneOldFiles deletes files older than days in dir (0 disables). Runs after
// each capture, replacing RN2's per-pass find -mtime sweeps (which hardcoded
// paths and, in prune_oldest.sh, had the -mtime sign inverted).
func pruneOldFiles(dir string, days int, log *slog.Logger) {
	if days <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				log.Info("pruned old recording", "file", e.Name())
			}
		}
	}
}

// scanCRorLF is a bufio.SplitFunc treating both \n and bare \r as line
// terminators (SatDump progress redraws).
func scanCRorLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
