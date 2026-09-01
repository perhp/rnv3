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
	"sync"
	"time"

	"github.com/perhp/rnv3/internal/cadu"
	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/livelog"
	"github.com/perhp/rnv3/internal/notify"
	"github.com/perhp/rnv3/internal/predict"
	"github.com/perhp/rnv3/internal/store"
)

// PostProcessor turns SatDump's raw products in workDir into final imagery
// (implemented by process.Pipeline; InventoryProcessor is the test/fallback
// implementation).
type PostProcessor interface {
	// Process returns the satellite images produced for the pass (absolute
	// paths); none means the pass failed.
	Process(ctx context.Context, p store.Pass, sat config.Satellite, workDir, fileBase string, daylight bool) ([]string, error)
	// UpdateAggregates rebuilds station-wide artifacts (sky map, daily
	// mosaics/timelapses). Called after the pass reached its terminal DB
	// state so the aggregates include it.
	UpdateAggregates(ctx context.Context, passStart time.Time)
}

// InventoryProcessor counts the PNGs SatDump produced without transforming
// them — drives the decoded/failed decision without the image pipeline.
type InventoryProcessor struct{}

func (InventoryProcessor) Process(_ context.Context, _ store.Pass, _ config.Satellite, workDir, _ string, _ bool) ([]string, error) {
	var found []string
	err := filepath.WalkDir(workDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".png") {
			found = append(found, path)
		}
		return nil
	})
	return found, err
}

func (InventoryProcessor) UpdateAggregates(context.Context, time.Time) {}

// PassNotifier receives decoded passes (notify.Notifier).
type PassNotifier interface {
	PassDecoded(ctx context.Context, ev notify.PassEvent)
}

// CADUContributor uploads Meteor recordings to the community composite
// service (notify.Notifier).
type CADUContributor interface {
	ContributeCADU(ctx context.Context, path string) error
}

// PassPublisher receives pass outcomes for the event webhooks
// (publish.Publisher).
type PassPublisher interface {
	PassDecoded(passID int64)
	PassFailed(passID int64)
}

// notifyTimeout bounds one pass's pushes (many images × slow APIs).
const notifyTimeout = 10 * time.Minute

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
	// Notify receives decoded passes; pushes run in the background so they
	// never delay the next capture. nil disables.
	Notify PassNotifier
	// Community uploads CADU recordings when enabled in config. nil disables.
	Community CADUContributor
	// Publish queues pass events for the webhook receivers. nil disables.
	Publish PassPublisher

	pushes sync.WaitGroup
	// Exec substitutes the process constructor in tests; nil means the real
	// satdump binary from Paths.SatdumpBinary.
	Exec func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// WaitPushes blocks until background notifications have finished (shutdown,
// tests).
func (r *Runner) WaitPushes() { r.pushes.Wait() }

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
	recording := r.handleRecording(ctx, cfg, p, sat, workDir, fileBase, log)

	images, procErr := r.Processor.Process(ctx, p, sat, workDir, fileBase, daylight)
	if procErr != nil {
		log.Error("post-processing failed", "err", procErr)
	}

	switch {
	case procErr != nil:
		// A partial product set is not a capture: the pipeline has already
		// discarded what it wrote, and nothing was registered.
		r.fail(p.ID, withExitInfo("post-processing failed: "+procErr.Error(), runErr))
		r.cleanupWorkDir(workDir, false)
	case len(images) > 0:
		maxSNR, avgSNR, ok := snr.Result()
		res := store.CaptureResult{FileBase: fileBase, Daylight: daylight, Gain: sat.Gain}
		if ok {
			res.MaxSNR, res.AvgSNR = &maxSNR, &avgSNR
		}
		if err := r.St.CompleteCapture(p.ID, res); err != nil {
			log.Error("cannot mark pass decoded", "err", err)
		}
		log.Info("pass decoded", "images", len(images), "daylight", daylight, "max_snr", maxSNR)
		r.live(fmt.Sprintf("=== pass decoded: %d images", len(images)))
		r.cleanupWorkDir(workDir, true)
		if r.Publish != nil {
			r.Publish.PassDecoded(p.ID)
		}
		r.push(notify.PassEvent{
			PassID: p.ID, Satellite: sat.Name, SatType: sat.Type, StartTS: p.StartTS, EndTS: p.EndTS,
			MaxElevation: p.MaxElevation, Direction: directionLabel(p.Direction), Side: sideLabel(p.AzimuthAtMax),
			SunElevation: sunEl, Gain: sat.Gain, Daylight: daylight, MaxSNR: res.MaxSNR, AvgSNR: res.AvgSNR,
			Images: images,
		})
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
func (r *Runner) handleRecording(ctx context.Context, cfg *config.Config, p store.Pass, sat config.Satellite, workDir, fileBase string, log *slog.Logger) bool {
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
		// The community upload runs in the background (a slow endpoint must
		// never hold up the next pass), so the recording is always moved out
		// of the work dir first and only deleted once the upload is done.
		contribute := cfg.Community.ContributeComposites && r.Community != nil
		retained := filepath.Join(cfg.Paths.AudioMeteor, fileBase+".cadu")
		switch {
		case cfg.Retention.DeleteMeteorAudio && !contribute:
			os.Remove(caduPath)
		default:
			if err := moveFile(caduPath, retained); err != nil {
				log.Error("cannot retain cadu", "err", err)
				break
			}
			if contribute {
				r.contribute(ctx, retained, cfg.Retention.DeleteMeteorAudio, log)
			}
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

// contributeTimeout bounds one community upload (recordings are large).
const contributeTimeout = 10 * time.Minute

// contribute uploads a retained recording in the background, deleting it
// afterwards when audio retention is off. ctx is the scheduler's, so a
// shutdown cancels the upload.
func (r *Runner) contribute(ctx context.Context, path string, deleteAfter bool, log *slog.Logger) {
	r.pushes.Add(1)
	go func() {
		defer r.pushes.Done()
		uctx, cancel := context.WithTimeout(ctx, contributeTimeout)
		defer cancel()
		log.Info("contributing recording to community composites", "file", filepath.Base(path))
		if err := r.Community.ContributeCADU(uctx, path); err != nil {
			log.Warn("community upload failed", "err", err)
		}
		if deleteAfter {
			os.Remove(path)
		}
	}()
}

// push hands a decoded pass to the notifier in the background.
func (r *Runner) push(ev notify.PassEvent) {
	if r.Notify == nil {
		return
	}
	r.pushes.Add(1)
	go func() {
		defer r.pushes.Done()
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		r.Notify.PassDecoded(ctx, ev)
	}()
}

func directionLabel(direction string) string {
	switch direction {
	case "northbound":
		return "Northbound"
	case "southbound":
		return "Southbound"
	}
	return direction
}

// sideLabel: E when the pass culminates in the eastern half of the sky.
func sideLabel(azimuthAtMax float64) string {
	if azimuthAtMax >= 0 && azimuthAtMax <= 180 {
		return "E"
	}
	return "W"
}

func (r *Runner) fail(id int64, reason string) {
	slog.Error("pass failed", "pass_id", id, "reason", reason)
	r.live("=== pass failed: " + reason)
	if err := r.St.SetPassState(id, store.StateFailed, reason); err != nil {
		slog.Error("cannot mark pass failed", "pass_id", id, "err", err)
	}
	if r.Publish != nil {
		r.Publish.PassFailed(id)
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
