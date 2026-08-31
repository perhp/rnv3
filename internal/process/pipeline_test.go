package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/predict"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

// testTLE writes a NOAA-19-shaped TLE cache the pipeline can load.
func writeTLECache(t *testing.T, dataDir string) {
	t.Helper()
	l1 := "1 33591U 09005A   26243.50000000  .00000100  00000-0  60000-4 0  999"
	l2 := "2 33591  99.1900 100.0000 0014000 120.0000 240.0000 14.1200000012345"
	l1 += string(rune('0' + tle.Checksum(l1)))
	l2 += string(rune('0' + tle.Checksum(l2)))
	dir := filepath.Join(dataDir, "tle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "NOAA 19\n" + l1 + "\n" + l2 + "\n"
	if err := os.WriteFile(filepath.Join(dir, "current.tle"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineEndToEndNOAA(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Default()
	cfg.Paths.Images = t.TempDir()
	cfg.Paths.Thumbs = t.TempDir()
	writeTLECache(t, dataDir)

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Use a real above-horizon window predicted from the cached TLE, so the
	// polar plot has an actual track to draw.
	set, _, err := tle.NewManager(dataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	obs := predict.Observer{Lat: cfg.Station.Latitude, Lon: cfg.Station.Longitude}
	epoch := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	passes, err := predict.Passes(set[33591], obs, epoch, epoch.Add(24*time.Hour))
	if err != nil || len(passes) == 0 {
		t.Fatalf("no predicted passes for pipeline test: %v", err)
	}
	aos, los := passes[0].AOS, passes[0].LOS
	if _, err := st.DB.Exec(`INSERT INTO passes (satellite, start_ts, end_ts, max_elevation, direction, state)
		VALUES ('NOAA 19', ?, ?, 55, 'southbound', 'processing')`,
		aos.Unix(), los.Unix()); err != nil {
		t.Fatal(err)
	}
	var passID int64
	if err := st.DB.QueryRow(`SELECT id FROM passes`).Scan(&passID); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_MSA.png"))
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_MCIR.png"))

	pl := &Pipeline{Prov: config.NewProvider(cfg), St: st, TLEs: tle.NewManager(dataDir)}
	sat := cfg.Satellites[2] // NOAA 19
	p := store.Pass{ID: passID, Satellite: "NOAA 19", StartTS: aos.Unix(), EndTS: los.Unix(),
		MaxElevation: 55, Direction: "southbound", State: store.StateProcessing}

	n, err := pl.Process(context.Background(), p, sat, work, "NOAA-19-20260831-123000", true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("produced %d images, want 2", n)
	}

	images, err := st.ImagesForPass(passID)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]store.Image{}
	for _, im := range images {
		byKind[im.Kind] = im
	}
	for _, want := range []string{"MSA", "MCIR", "website-thumbnail", "polar-azel", "polar-direction"} {
		if _, ok := byKind[want]; !ok {
			t.Errorf("kind %q not registered; have %v", want, byKind)
		}
	}

	// The daylight website thumbnail must prefer MSA.
	wt := filepath.Join(cfg.Paths.Thumbs, "NOAA-19-20260831-123000-website-thumbnail.jpg")
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("website thumbnail missing: %v", err)
	}
	// Polar SVGs exist on disk.
	for _, f := range []string{
		filepath.Join(cfg.Paths.Images, "NOAA-19-20260831-123000-polar-azel.svg"),
		filepath.Join(cfg.Paths.Images, "NOAA-19-20260831-123000-polar-direction.svg"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("artifact missing: %s", f)
		}
	}

	// Aggregates are NOT written by Process (the pass is not terminal yet) —
	// only by UpdateAggregates, which the runner calls after CompleteCapture.
	if _, err := os.Stat(filepath.Join(cfg.Paths.Images, SkymapFilename)); err == nil {
		t.Error("sky map must not be written before the pass is terminal")
	}
	pl.UpdateAggregates(context.Background(), aos)
	if _, err := os.Stat(filepath.Join(cfg.Paths.Images, SkymapFilename)); err != nil {
		t.Errorf("sky map missing after UpdateAggregates: %v", err)
	}
}

func TestPipelineNoImagesShortCircuits(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.Images = t.TempDir()
	cfg.Paths.Thumbs = t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pl := &Pipeline{Prov: config.NewProvider(cfg), St: st, TLEs: tle.NewManager(t.TempDir())}
	n, err := pl.Process(context.Background(), store.Pass{ID: 1, Direction: "southbound"},
		cfg.Satellites[2], t.TempDir(), "X", true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("empty work dir produced %d images", n)
	}
	if _, err := os.Stat(filepath.Join(cfg.Paths.Images, SkymapFilename)); err == nil {
		t.Error("Process must not produce aggregate artifacts")
	}
	// The runner still updates aggregates after marking the pass failed, so
	// failures appear on the sky map immediately.
	pl.UpdateAggregates(context.Background(), time.Now())
	if _, err := os.Stat(filepath.Join(cfg.Paths.Images, SkymapFilename)); err != nil {
		t.Errorf("sky map missing after UpdateAggregates: %v", err)
	}
}
