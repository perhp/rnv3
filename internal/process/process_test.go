package process

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writeTestPNG creates a 16x16 image with a red 8x8 top-left quadrant (flip
// marker, large enough to survive JPEG chroma subsampling) on blue.
func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if x < 8 && y < 8 {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				img.Set(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// isRedTopLeft reports whether the JPEG at path still has its red marker in
// the top-left (i.e. was NOT flipped). JPEG is lossy; compare dominant channel.
func isRedTopLeft(t *testing.T, path string) bool {
	t.Helper()
	img, err := loadImage(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	b0 := img.Bounds()
	r, _, b, _ := img.At(b0.Min.X+2, b0.Min.Y+2).RGBA()
	return r > b
}

func kinds(produced []Produced) map[string]bool {
	m := map[string]bool{}
	for _, p := range produced {
		m[p.Kind] = true
	}
	return m
}

func TestProcessNOAARules(t *testing.T) {
	work := t.TempDir()
	images := t.TempDir()
	thumbs := t.TempDir()

	// Real-shaped SatDump NOAA output names.
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_MCIR.png"))
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_MCIR_(Uncalibrated).png")) // calibrated exists → dropped
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_MSA_(Uncalibrated).png"))  // no calibrated → promoted
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_ZA_enhancement.png"))
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_Thermal_Channel_(channel_4).png"))
	writeTestPNG(t, filepath.Join(work, "rgb_avhrr_3_rgb_221.png"))
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_HVC.png"))
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_HVC_map.png")) // map variant wins

	produced, err := processNOAA(work, images, thumbs, "NOAA-19-20260901-120000", false, 90)
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(produced)
	for _, want := range []string{"MCIR", "MSA", "ZA", "Thermal_Channel", "221", "HVC"} {
		if !got[want] {
			t.Errorf("missing kind %q in %v", want, got)
		}
	}
	if len(produced) != 6 {
		t.Errorf("produced %d kinds, want 6: %v", len(produced), got)
	}
	// Every product has a real file + thumbnail.
	for _, p := range produced {
		if _, err := os.Stat(filepath.Join(images, p.ImageName)); err != nil {
			t.Errorf("image missing: %s", p.ImageName)
		}
		if _, err := os.Stat(filepath.Join(thumbs, p.ThumbName)); err != nil {
			t.Errorf("thumb missing: %s", p.ThumbName)
		}
	}
}

func TestProcessNOAAFlipRules(t *testing.T) {
	work := t.TempDir()
	images := t.TempDir()
	thumbs := t.TempDir()
	writeTestPNG(t, filepath.Join(work, "avhrr_apt_rgb_MCIR.png"))  // flipped on northbound
	writeTestPNG(t, filepath.Join(work, "rgb_avhrr_3_rgb_221.png")) // rgb_*: already north-up, NOT flipped

	if _, err := processNOAA(work, images, thumbs, "N", true, 90); err != nil {
		t.Fatal(err)
	}
	if isRedTopLeft(t, filepath.Join(images, "N-MCIR.jpg")) {
		t.Error("northbound non-rgb image was not flipped")
	}
	if !isRedTopLeft(t, filepath.Join(images, "N-221.jpg")) {
		t.Error("rgb_ composite must not be flipped (SatDump emits it north-up)")
	}

	// Southbound: nothing flips.
	work2, images2 := t.TempDir(), t.TempDir()
	writeTestPNG(t, filepath.Join(work2, "avhrr_apt_rgb_MCIR.png"))
	if _, err := processNOAA(work2, images2, thumbs, "S", false, 90); err != nil {
		t.Fatal(err)
	}
	if !isRedTopLeft(t, filepath.Join(images2, "S-MCIR.jpg")) {
		t.Error("southbound image must not be flipped")
	}
}

func TestProcessMeteorRules(t *testing.T) {
	work := t.TempDir()
	images := t.TempDir()
	thumbs := t.TempDir()
	filled := filepath.Join(work, "MSU-MR (Filled)")
	plain := filepath.Join(work, "MSU-MR")

	// Gap-filled version in Filled must survive the no-clobber copy from MSU-MR.
	writeTestPNG(t, filepath.Join(filled, "msu_mr_rgb_MSA_corrected.png"))
	writeTestPNG(t, filepath.Join(plain, "msu_mr_rgb_MSA_corrected.png"))
	// Only-in-MSU-MR product gets copied.
	writeTestPNG(t, filepath.Join(plain, "msu_mr_rgb_321_corrected.png"))
	// Non-composite clutter is pruned.
	writeTestPNG(t, filepath.Join(filled, "msu_mr_avhrr_something_raw.png"))
	// Raw channel image is kept.
	writeTestPNG(t, filepath.Join(filled, "MSU-MR-4.png"))
	// Projected composite without corrected counterpart → deleted.
	writeTestPNG(t, filepath.Join(filled, "rgb_msu_mr_rgb_456_projected.png"))
	// Projected composite with corrected counterpart → kept.
	writeTestPNG(t, filepath.Join(filled, "rgb_msu_mr_rgb_221_projected.png"))
	writeTestPNG(t, filepath.Join(filled, "msu_mr_rgb_221_corrected.png"))
	// Equirect corrected duplicate → deleted; its projected sibling kept.
	writeTestPNG(t, filepath.Join(filled, "msu_mr_rgb_221_equirect_corrected.png"))

	produced, err := processMeteor(work, images, thumbs, "M", false, true, 90)
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(produced)
	for _, want := range []string{"MSA_corrected", "321_corrected", "MSU-MR-4", "221_projected", "221_corrected"} {
		if !got[want] {
			t.Errorf("missing kind %q in %v", want, got)
		}
	}
	for _, banned := range []string{"456_projected", "221_equirect_corrected", "avhrr_something_raw"} {
		if got[banned] {
			t.Errorf("kind %q should have been pruned: %v", banned, got)
		}
	}
}

func TestProcessMeteorFlipOnlyCorrectedAndRaw(t *testing.T) {
	work := t.TempDir()
	images := t.TempDir()
	thumbs := t.TempDir()
	filled := filepath.Join(work, "MSU-MR (Filled)")
	writeTestPNG(t, filepath.Join(filled, "msu_mr_rgb_MSA_corrected.png"))
	writeTestPNG(t, filepath.Join(filled, "msu_mr_rgb_MSA_projected.png"))
	writeTestPNG(t, filepath.Join(filled, "MSU-MR-2.png"))

	if _, err := processMeteor(work, images, thumbs, "M", true, true, 90); err != nil {
		t.Fatal(err)
	}
	if isRedTopLeft(t, filepath.Join(images, "M-MSA_corrected.jpg")) {
		t.Error("corrected image must flip on northbound")
	}
	if isRedTopLeft(t, filepath.Join(images, "M-MSU-MR-2.jpg")) {
		t.Error("raw channel image must flip on northbound")
	}
	if !isRedTopLeft(t, filepath.Join(images, "M-MSA_projected.jpg")) {
		t.Error("projected image is map-anchored and must never flip")
	}

	// flip_northbound=false suppresses the flip entirely.
	work2, images2 := t.TempDir(), t.TempDir()
	writeTestPNG(t, filepath.Join(work2, "MSU-MR (Filled)", "msu_mr_rgb_MSA_corrected.png"))
	if _, err := processMeteor(work2, images2, thumbs, "M2", true, false, 90); err != nil {
		t.Fatal(err)
	}
	if !isRedTopLeft(t, filepath.Join(images2, "M2-MSA_corrected.jpg")) {
		t.Error("flip disabled in config but image was flipped")
	}
}
