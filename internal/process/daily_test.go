package process

import (
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

// writeSolidJPEG writes a WxH image of one color.
func writeSolidJPEG(t *testing.T, path string, w, h int, c color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	if err := saveJPEG(path, img, 95); err != nil {
		t.Fatal(err)
	}
}

func dailySetup(t *testing.T) (*config.Config, *store.Store, time.Time) {
	t.Helper()
	cfg := config.Default()
	cfg.Paths.Images = t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	day := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	return cfg, st, day
}

// seedDecoded inserts a decoded pass with a file base and optional SNR.
func seedDecoded(t *testing.T, st *store.Store, base string, start time.Time, snr *float64, daylight bool) {
	t.Helper()
	d := 0
	if daylight {
		d = 1
	}
	var snrVal any
	if snr != nil {
		snrVal = *snr
	}
	if _, err := st.DB.Exec(`INSERT INTO passes
		(satellite, start_ts, end_ts, max_elevation, state, file_base, daylight, max_snr)
		VALUES (?, ?, ?, 50, 'decoded', ?, ?, ?)`,
		base, start.Unix(), start.Add(15*time.Minute).Unix(), base, d, snrVal); err != nil {
		t.Fatal(err)
	}
}

func TestMosaicLightenBlend(t *testing.T) {
	cfg, st, day := dailySetup(t)
	cfg.Daily.Mosaic.Enabled = true
	cfg.Daily.Mosaic.Suffixes = []string{"-321_projected.jpg"}

	seedDecoded(t, st, "A", day.Add(-2*time.Hour), nil, true)
	seedDecoded(t, st, "B", day.Add(-1*time.Hour), nil, true)
	// A: red left half; B: green right half — blend keeps the brighter pixel.
	imgA := image.NewRGBA(image.Rect(0, 0, 8, 4))
	imgB := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			if x < 4 {
				imgA.Set(x, y, color.RGBA{200, 0, 0, 255})
				imgB.Set(x, y, color.RGBA{0, 0, 0, 255})
			} else {
				imgA.Set(x, y, color.RGBA{0, 0, 0, 255})
				imgB.Set(x, y, color.RGBA{0, 200, 0, 255})
			}
		}
	}
	if err := saveJPEG(filepath.Join(cfg.Paths.Images, "A-321_projected.jpg"), imgA, 95); err != nil {
		t.Fatal(err)
	}
	if err := saveJPEG(filepath.Join(cfg.Paths.Images, "B-321_projected.jpg"), imgB, 95); err != nil {
		t.Fatal(err)
	}

	if err := BuildDailyArtifacts(cfg, st, day); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(cfg.Paths.Images, "mosaic-20260901-321_projected.jpg")
	img, err := loadImage(out)
	if err != nil {
		t.Fatalf("mosaic not written: %v", err)
	}
	r, _, _, _ := img.At(1, 1).RGBA()
	_, g, _, _ := img.At(6, 1).RGBA()
	if r>>8 < 100 {
		t.Errorf("left half lost frame A's red channel: %d", r>>8)
	}
	if g>>8 < 100 {
		t.Errorf("right half lost frame B's green channel: %d", g>>8)
	}
}

func TestTimelapseGIF(t *testing.T) {
	cfg, st, day := dailySetup(t)
	cfg.Daily.Timelapse.Enabled = true
	cfg.Daily.Timelapse.Suffixes = []string{"-221_projected.jpg"}

	for i, base := range []string{"A", "B", "C"} {
		seedDecoded(t, st, base, day.Add(time.Duration(i)*time.Hour), nil, true)
		writeSolidJPEG(t, filepath.Join(cfg.Paths.Images, base+"-221_projected.jpg"),
			16, 8, color.RGBA{uint8(60 * (i + 1)), 0, 0, 255})
	}
	if err := BuildDailyArtifacts(cfg, st, day); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(cfg.Paths.Images, "timelapse-20260901-221_projected.gif")
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("timelapse not written: %v", err)
	}
	defer f.Close()
	g, err := gif.DecodeAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Image) != 3 {
		t.Errorf("gif has %d frames, want 3", len(g.Image))
	}
	if g.LoopCount != 0 {
		t.Errorf("gif must loop forever, LoopCount=%d", g.LoopCount)
	}
}

func TestTimelapseSkipsSingleFrame(t *testing.T) {
	cfg, st, day := dailySetup(t)
	cfg.Daily.Timelapse.Enabled = true
	cfg.Daily.Timelapse.Suffixes = []string{"-221_projected.jpg"}
	seedDecoded(t, st, "A", day, nil, true)
	writeSolidJPEG(t, filepath.Join(cfg.Paths.Images, "A-221_projected.jpg"), 8, 8, color.RGBA{99, 0, 0, 255})

	if err := BuildDailyArtifacts(cfg, st, day); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Paths.Images, "timelapse-20260901-221_projected.gif")); err == nil {
		t.Error("single-frame timelapse should not be written")
	}
}

func TestMosaicFilters(t *testing.T) {
	cfg, st, day := dailySetup(t)
	cfg.Daily.Mosaic.Enabled = true
	cfg.Daily.Mosaic.Suffixes = []string{"-221_projected.jpg"}
	cfg.Daily.Mosaic.MinSNR = 10
	cfg.Daily.Mosaic.DaylightOnly = true

	lowSNR, highSNR := 3.0, 15.0
	seedDecoded(t, st, "LOW", day.Add(-3*time.Hour), &lowSNR, true)     // filtered: SNR
	seedDecoded(t, st, "NIGHT", day.Add(-2*time.Hour), &highSNR, false) // filtered: night
	seedDecoded(t, st, "NOSNR", day.Add(-1*time.Hour), nil, true)       // kept: NULL SNR always passes
	seedDecoded(t, st, "GOOD", day, &highSNR, true)                     // kept

	for _, base := range []string{"LOW", "NIGHT", "NOSNR", "GOOD"} {
		writeSolidJPEG(t, filepath.Join(cfg.Paths.Images, base+"-221_projected.jpg"), 8, 8, color.RGBA{200, 200, 200, 255})
	}
	frames := collectFrames(cfg.Paths.Images,
		mustDecoded(t, st, day), "-221_projected.jpg", cfg.Daily.Mosaic.MinSNR, cfg.Daily.Mosaic.DaylightOnly)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (NOSNR + GOOD): %v", len(frames), frames)
	}
}

func mustDecoded(t *testing.T, st *store.Store, day time.Time) []store.DecodedPass {
	t.Helper()
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	dayEnd := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, day.Location())
	passes, err := st.DecodedPassesBetween(dayStart, dayEnd)
	if err != nil {
		t.Fatal(err)
	}
	return passes
}

func TestMosaicSkipsSingleFrame(t *testing.T) {
	cfg, st, day := dailySetup(t)
	cfg.Daily.Mosaic.Enabled = true
	cfg.Daily.Mosaic.Suffixes = []string{"-321_projected.jpg"}
	seedDecoded(t, st, "A", day, nil, true)
	writeSolidJPEG(t, filepath.Join(cfg.Paths.Images, "A-321_projected.jpg"), 8, 8, color.RGBA{99, 0, 0, 255})

	if err := BuildDailyArtifacts(cfg, st, day); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Paths.Images, "mosaic-20260901-321_projected.jpg")); err == nil {
		t.Error("single-frame mosaic should not be written (it is just a copy of the pass)")
	}
}
