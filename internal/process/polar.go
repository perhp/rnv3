package process

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/predict"
)

// Polar pass plots as SVG — replaces RN2's matplotlib polar_plot.py.
// Two variants for parity with the old panel: "azel" (the raw track) and
// "direction" (the same track annotated with AOS/LOS at the configured
// minimum elevation and compass labels).

const polarSize = 480.0

type polarStyle struct {
	bg, grid, ink, track string
	start, end, max      string
}

// Fixed, theme-independent palette in the ops-console spirit.
var polarColors = polarStyle{
	bg: "#fbfaf7", grid: "#d8d2c4", ink: "#6f6a5e", track: "#22251f",
	start: "#3d7a3a", end: "#a13232", max: "#a4741b",
}

// polarXY maps azimuth/elevation to plot coordinates: north up, east right,
// zenith centered.
func polarXY(az, el float64) (float64, float64) {
	c := polarSize / 2
	r := (polarSize/2 - 40) * (90 - clamp(el, 0, 90)) / 90
	rad := az * math.Pi / 180
	return c + r*math.Sin(rad), c - r*math.Cos(rad)
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// PolarPlot renders one pass-track SVG. variant is "azel" or "direction";
// minElevation is only used by the direction variant (AOS/LOS markers).
func PolarPlot(variant, satName string, track []predict.Sample, direction string, minElevation float64, outPath string) error {
	var above []predict.Sample
	for _, s := range track {
		if s.Elevation >= 0 {
			above = append(above, s)
		}
	}
	if len(above) < 2 {
		return fmt.Errorf("track has fewer than 2 above-horizon samples")
	}
	maxIdx := 0
	for i, s := range above {
		if s.Elevation > above[maxIdx].Elevation {
			maxIdx = i
		}
	}

	var b strings.Builder
	c := polarSize / 2
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %[1]v %[1]v" font-family="Cascadia Mono, ui-monospace, monospace">`, polarSize)
	fmt.Fprintf(&b, `<rect width="%[1]v" height="%[1]v" fill="%s"/>`, polarSize, polarColors.bg)

	// Elevation rings at 0/30/60 with labels.
	for _, el := range []float64{0, 30, 60} {
		r := (polarSize/2 - 40) * (90 - el) / 90
		fmt.Fprintf(&b, `<circle cx="%v" cy="%v" r="%.1f" fill="none" stroke="%s" stroke-width="1"/>`, c, c, r, polarColors.grid)
		fmt.Fprintf(&b, `<text x="%v" y="%.1f" font-size="10" fill="%s" text-anchor="middle">%.0f°</text>`, c, c-r-3, polarColors.ink, el)
	}
	// Compass spokes.
	compass := []struct {
		label string
		az    float64
	}{{"N", 0}, {"NE", 45}, {"E", 90}, {"SE", 135}, {"S", 180}, {"SW", 225}, {"W", 270}, {"NW", 315}}
	for _, p := range compass {
		x1, y1 := polarXY(p.az, 90)
		x2, y2 := polarXY(p.az, 0)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="0.5"/>`, x1, y1, x2, y2, polarColors.grid)
		lx, ly := polarXY(p.az, -6) // just outside the horizon ring
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="%s" text-anchor="middle" dominant-baseline="middle">%s</text>`, lx, ly, polarColors.ink, p.label)
	}

	// The track.
	var pts strings.Builder
	for i, s := range above {
		x, y := polarXY(s.Azimuth, s.Elevation)
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", x, y)
	}
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2"/>`, pts.String(), polarColors.track)

	// Markers: start, end, max elevation.
	sx, sy := polarXY(above[0].Azimuth, above[0].Elevation)
	ex, ey := polarXY(above[len(above)-1].Azimuth, above[len(above)-1].Elevation)
	mx, my := polarXY(above[maxIdx].Azimuth, above[maxIdx].Elevation)
	if variant == "direction" {
		// AOS/LOS at the station's minimum scheduling elevation.
		aos, los := above[0], above[len(above)-1]
		for _, s := range above {
			if s.Elevation >= minElevation {
				aos = s
				break
			}
		}
		for i := len(above) - 1; i >= 0; i-- {
			if above[i].Elevation >= minElevation {
				los = above[i]
				break
			}
		}
		sx, sy = polarXY(aos.Azimuth, aos.Elevation)
		ex, ey = polarXY(los.Azimuth, los.Elevation)
	}
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="6" fill="%s"><title>start</title></circle>`, sx, sy, polarColors.start)
	fmt.Fprintf(&b, `<path d="M %.1f %.1f l 5 5 m -10 0 l 10 -10 m 0 10 l -10 -10" stroke="%s" stroke-width="2.5" fill="none"><title>end</title></path>`, ex-0, ey-0, polarColors.end)
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="5" fill="%s" stroke="%s" stroke-width="1.5"><title>max elevation</title></circle>`, mx, my, polarColors.max, polarColors.bg)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" font-weight="bold" fill="%s" text-anchor="middle">%.0f°</text>`,
		mx, my+18, polarColors.max, above[maxIdx].Elevation)

	// Title block.
	title := "Azimuth / Elevation"
	if variant == "direction" {
		title = "Pass Direction"
	}
	fmt.Fprintf(&b, `<text x="10" y="16" font-size="12" fill="%s">%s — %s</text>`, polarColors.ink, escapeXML(satName), title)
	fmt.Fprintf(&b, `<text x="10" y="30" font-size="10" fill="%s">%s · max %.0f° · %s</text>`,
		polarColors.ink, above[0].Time.UTC().Format("2006-01-02 15:04:05Z"), above[maxIdx].Elevation, escapeXML(direction))
	b.WriteString(`</svg>`)

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(outPath, []byte(b.String()))
}

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// WriteFileAtomic writes via a temp file + rename so a crash never leaves a
// half-written artifact (same pattern as RN2's sky map / daily imagery).
func WriteFileAtomic(path string, data []byte) error {
	tmp := path + fmt.Sprintf(".%d.tmp", time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows: rename won't clobber an existing file.
		os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}
