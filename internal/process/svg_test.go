package process

import (
	"encoding/xml"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/predict"
	"github.com/perhp/rnv3/internal/store"
)

func syntheticTrack() []predict.Sample {
	// Rises in the SW, peaks at 65°, sets in the NE.
	var track []predict.Sample
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	n := 600
	for i := 0; i <= n; i++ {
		frac := float64(i) / float64(n)
		el := -5 + 70*(1-4*(frac-0.5)*(frac-0.5)) // parabola peaking mid-pass
		az := 225 + 180*frac                      // SW → NE sweep
		if az >= 360 {
			az -= 360
		}
		track = append(track, predict.Sample{Time: t0.Add(time.Duration(i) * time.Second), Azimuth: az, Elevation: el})
	}
	return track
}

func TestPolarPlotSVG(t *testing.T) {
	dir := t.TempDir()
	for _, variant := range []string{"azel", "direction"} {
		out := filepath.Join(dir, variant+".svg")
		if err := PolarPlot(variant, "NOAA 19", syntheticTrack(), "northbound", 30, out); err != nil {
			t.Fatalf("%s: %v", variant, err)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		svg := string(raw)
		var doc struct{ XMLName xml.Name }
		if err := xml.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s: not well-formed XML: %v", variant, err)
		}
		for _, want := range []string{"<polyline", "NOAA 19", "northbound", "65°"} {
			if !strings.Contains(svg, want) {
				t.Errorf("%s: missing %q", variant, want)
			}
		}
	}
}

// The two variants use opposite radial axes (RN2: rlim(0, 92) for az/el,
// rlim(90, 0) for direction): a high-elevation point sits near the rim on the
// az/el plot and near the centre on the direction plot.
func TestPolarVariantsUseOppositeRadialAxes(t *testing.T) {
	c := polarSize / 2
	dist := func(x, y float64) float64 { return math.Hypot(x-c, y-c) }

	ax, ay := polarPoint("azel", 90, 80)
	dx, dy := polarPoint("direction", 90, 80)
	if dist(ax, ay) <= dist(dx, dy) {
		t.Errorf("az/el should put 80° near the rim: azel r=%.1f, direction r=%.1f", dist(ax, ay), dist(dx, dy))
	}
	if r := polarRadius("azel", 0); r != 0 {
		t.Errorf("az/el horizon should be at the centre, got r=%.1f", r)
	}
	if r := polarRadius("direction", 90); r != 0 {
		t.Errorf("direction zenith should be at the centre, got r=%.1f", r)
	}
	// Both agree on azimuth orientation: east is to the right.
	if ax <= c || dx <= c {
		t.Errorf("east should be right of centre: azel x=%.1f, direction x=%.1f", ax, dx)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "azel.svg")
	if err := PolarPlot("azel", "NOAA 19", syntheticTrack(), "northbound", 30, out); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	svg := string(raw)
	if strings.Contains(svg, ">NE<") || !strings.Contains(svg, ">45°<") {
		t.Error("az/el plot should label spokes in degrees, not compass points")
	}
	if !strings.Contains(svg, ">80°<") {
		t.Error("az/el plot should have an 80° elevation ring")
	}
}

func TestPolarPlotRejectsEmptyTrack(t *testing.T) {
	err := PolarPlot("azel", "X", []predict.Sample{{Elevation: -10}, {Elevation: -20}}, "southbound", 30,
		filepath.Join(t.TempDir(), "x.svg"))
	if err == nil {
		t.Fatal("below-horizon track accepted")
	}
}

func TestSkymapSVG(t *testing.T) {
	dir := t.TempDir()
	snr := 12.5
	points := []store.SkymapPoint{
		{AzimuthAtMax: 90, MaxElevation: 60, MaxSNR: &snr},
		{AzimuthAtMax: 200, MaxElevation: 30},               // hollow (no SNR)
		{AzimuthAtMax: 310, MaxElevation: 45, Failed: true}, // red cross
	}
	if err := WriteSkymap(points, dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, SkymapFilename))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ XMLName xml.Name }
	if err := xml.Unmarshal(raw, &doc); err != nil {
		t.Errorf("skymap not well-formed XML: %v", err)
	}
	svg := string(raw)
	if !strings.Contains(svg, "3 passes") {
		t.Error("pass count missing from legend")
	}
	if !strings.Contains(svg, "SNR 12.5 dB") {
		t.Error("SNR tooltip missing")
	}
}

func TestSnrColorRange(t *testing.T) {
	if got := snrColor(0, 20); got != "#b7cdec" {
		t.Errorf("0%% color = %s", got)
	}
	if got := snrColor(20, 20); got != "#123f79" {
		t.Errorf("100%% color = %s", got)
	}
}
