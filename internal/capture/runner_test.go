package capture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

// TestHelperProcess doubles as the fake satdump binary: the runner test
// re-execs the test binary with RNV3_FAKE_SATDUMP set, and this "test"
// mimics SatDump's observable behavior (stdout SNR lines, product files in
// the work dir passed as the third pipeline argument).
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("RNV3_FAKE_SATDUMP")
	if mode == "" {
		return
	}
	// os.Args after "--": the satdump argument list; args[2] is the work dir.
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) < 3 {
		os.Exit(2)
	}
	workDir := args[2]

	os.Stdout.WriteString("[INFO] Starting fake satdump\r\n")
	os.Stdout.WriteString("Viterbi BER: 0.01, SNR: 11.5 dB\n")
	os.Stdout.WriteString("progress redraw\rSNR : 8.5\n")

	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(workDir, name), data, 0o644); err != nil {
			os.Exit(2)
		}
	}
	switch mode {
	case "noaa-ok":
		write("noaa_apt.wav", []byte("RIFF-fake-audio"))
		write("avhrr_apt_rgb_MCIR.png", []byte("png1"))
		write("avhrr_apt_rgb_MSA.png", []byte("png2"))
	case "meteor-ok":
		var cadus []byte
		for i := 0; i < 4; i++ {
			f := make([]byte, 1024)
			copy(f, []byte{0x1A, 0xCF, 0xFC, 0x1D})
			f[5] = 5
			counter := 100 + i
			if i >= 2 {
				counter += 2 // one 2-frame gap: 100,101,104,105
			}
			f[8] = byte(counter)
			cadus = append(cadus, f...)
		}
		write("meteor_m2-x_lrpt.cadu", cadus)
		write("msu_mr_rgb_221_corrected.png", []byte("png"))
	case "no-images":
		write("noaa_apt.wav", []byte("RIFF-fake-audio"))
	case "no-recording":
		// produce nothing
	}
	os.Exit(0)
}

func fakeExec(t *testing.T, mode string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	t.Helper()
	t.Setenv("RNV3_FAKE_SATDUMP", mode)
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		full := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		return exec.CommandContext(ctx, os.Args[0], full...)
	}
}

func testRunner(t *testing.T, mode string, sat config.Satellite) (*Runner, *config.Config, *store.Store, store.Pass) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Paths.Work = filepath.Join(dir, "work")
	cfg.Paths.Ramfs = filepath.Join(dir, "no-ramfs") // does not exist → disk path
	cfg.Paths.AudioNOAA = filepath.Join(dir, "audio", "noaa")
	cfg.Paths.AudioMeteor = filepath.Join(dir, "audio", "meteor")

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now()
	plan := []store.Pass{{
		Satellite:    sat.Name,
		StartTS:      now.Add(1 * time.Minute).Unix(),
		EndTS:        now.Add(2 * time.Minute).Unix(),
		MaxElevation: 55,
		Direction:    "southbound",
		State:        store.StateScheduled,
	}}
	if err := st.ReplaceFuturePlan(now, plan); err != nil {
		t.Fatal(err)
	}
	p, err := st.NextScheduled(now)
	if err != nil || p == nil {
		t.Fatalf("no scheduled pass: %v", err)
	}

	r := &Runner{Prov: config.NewProvider(cfg), St: st, Processor: InventoryProcessor{}, Exec: fakeExec(t, mode)}
	return r, cfg, st, *p
}

func passState(t *testing.T, st *store.Store, id int64) (state, errText string) {
	t.Helper()
	var e *string
	if err := st.DB.QueryRow(`SELECT state, error_text FROM passes WHERE id = ?`, id).Scan(&state, &e); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		errText = *e
	}
	return
}

func TestRunnerNOAADecodes(t *testing.T) {
	r, cfg, st, p := testRunner(t, "noaa-ok", config.Default().Satellites[2]) // NOAA 19
	r.Run(context.Background(), p, cfg.Satellites[2])

	state, _ := passState(t, st, p.ID)
	if state != store.StateDecoded {
		t.Fatalf("state = %s, want decoded", state)
	}

	var maxSNR, avgSNR float64
	var fileBase string
	var daylight int
	if err := st.DB.QueryRow(`SELECT max_snr, avg_snr, file_base, daylight FROM passes WHERE id = ?`, p.ID).
		Scan(&maxSNR, &avgSNR, &fileBase, &daylight); err != nil {
		t.Fatal(err)
	}
	if maxSNR != 11.5 || avgSNR != 10.0 {
		t.Errorf("snr = %v/%v, want 11.5/10.0", maxSNR, avgSNR)
	}
	if fileBase == "" {
		t.Error("file_base not set")
	}

	// wav retained under the file base; work dir cleaned up.
	wav := filepath.Join(cfg.Paths.AudioNOAA, fileBase+".wav")
	if _, err := os.Stat(wav); err != nil {
		t.Errorf("retained wav missing: %v", err)
	}
	if entries, _ := os.ReadDir(cfg.Paths.Work); len(entries) != 0 {
		t.Errorf("work dir not cleaned after success: %v", entries)
	}
}

func TestRunnerMeteorFrameStats(t *testing.T) {
	r, cfg, st, p := testRunner(t, "meteor-ok", config.Default().Satellites[3]) // METEOR-M2 3
	r.Run(context.Background(), p, cfg.Satellites[3])

	state, _ := passState(t, st, p.ID)
	if state != store.StateDecoded {
		t.Fatalf("state = %s, want decoded", state)
	}
	var received, expected, gap int
	var loss float64
	if err := st.DB.QueryRow(`SELECT frames_received, frames_expected, frame_loss_pct, largest_frame_gap
		FROM passes WHERE id = ?`, p.ID).Scan(&received, &expected, &loss, &gap); err != nil {
		t.Fatal(err)
	}
	if received != 4 || expected != 6 || gap != 2 {
		t.Errorf("frame stats %d/%d gap %d, want 4/6 gap 2", received, expected, gap)
	}
	cadu := filepath.Join(cfg.Paths.AudioMeteor, FileBase(p.Satellite, time.Unix(p.StartTS, 0))+".cadu")
	if _, err := os.Stat(cadu); err != nil {
		t.Errorf("retained cadu missing: %v", err)
	}
}

func TestRunnerNoImagesFails(t *testing.T) {
	r, cfg, st, p := testRunner(t, "no-images", config.Default().Satellites[2])
	r.Run(context.Background(), p, cfg.Satellites[2])
	state, errText := passState(t, st, p.ID)
	if state != store.StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}
	if errText != "decoder produced no images" {
		t.Errorf("error = %q", errText)
	}
	// Work dir kept for debugging on failure.
	if entries, _ := os.ReadDir(cfg.Paths.Work); len(entries) == 0 {
		t.Error("work dir should be kept after failure")
	}
}

func TestRunnerNoRecordingFails(t *testing.T) {
	r, cfg, st, p := testRunner(t, "no-recording", config.Default().Satellites[2])
	r.Run(context.Background(), p, cfg.Satellites[2])
	state, errText := passState(t, st, p.ID)
	if state != store.StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}
	if errText != "no recording produced" {
		t.Errorf("error = %q", errText)
	}
}

// stateCheckProcessor records the pass state as seen from UpdateAggregates,
// proving the runner finalizes the DB row before aggregates rebuild.
type stateCheckProcessor struct {
	InventoryProcessor
	st        *store.Store
	passID    int64
	seenState string
}

func (p *stateCheckProcessor) UpdateAggregates(_ context.Context, _ time.Time) {
	p.st.DB.QueryRow(`SELECT state FROM passes WHERE id = ?`, p.passID).Scan(&p.seenState)
}

func TestRunnerAggregatesRunAfterTerminalState(t *testing.T) {
	// Decoded path.
	r, cfg, st, p := testRunner(t, "noaa-ok", config.Default().Satellites[2])
	proc := &stateCheckProcessor{st: st, passID: p.ID}
	r.Processor = proc
	r.Run(context.Background(), p, cfg.Satellites[2])
	if proc.seenState != store.StateDecoded {
		t.Errorf("UpdateAggregates saw state %q, want decoded", proc.seenState)
	}

	// Failed path: aggregates still run, after the failed state landed.
	r2, cfg2, st2, p2 := testRunner(t, "no-recording", config.Default().Satellites[2])
	proc2 := &stateCheckProcessor{st: st2, passID: p2.ID}
	r2.Processor = proc2
	r2.Run(context.Background(), p2, cfg2.Satellites[2])
	if proc2.seenState != store.StateFailed {
		t.Errorf("UpdateAggregates saw state %q, want failed", proc2.seenState)
	}
}

func TestRunnerLateFireFails(t *testing.T) {
	r, cfg, st, p := testRunner(t, "noaa-ok", config.Default().Satellites[2])
	p.EndTS = time.Now().Unix() - 10 // window already over
	r.Run(context.Background(), p, cfg.Satellites[2])
	state, _ := passState(t, st, p.ID)
	if state != store.StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}
}
