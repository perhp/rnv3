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
// Two variants for parity with the old panel, and they differ in the radial
// axis, not just the markers:
//
//   - "azel" is RN2's constructAzElPlot: rlim(0, 92) — the horizon is the
//     centre and the zenith the rim, so the track sweeps out from the middle
//     and back. Angular ticks are plain degrees (0°, 45°, …), rings every 20°.
//   - "direction" is constructDirectionPlot: rlim(90, 0) — the sky view with
//     the zenith centred and the horizon at the rim, compass labels, and
//     AOS/LOS markers at the configured minimum elevation.

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

// polarRimRadius is the radius of the outermost ring (the horizon in the sky
// view, the zenith in the az/el view).
const polarRimRadius = polarSize/2 - 40

// polarAt maps an azimuth and a plot radius to coordinates: north up, east
// right (clockwise azimuth), centred on the plot.
func polarAt(az, r float64) (float64, float64) {
	c := polarSize / 2
	rad := az * math.Pi / 180
	return c + r*math.Sin(rad), c - r*math.Cos(rad)
}

// polarXY maps azimuth/elevation to sky-view coordinates: zenith centred,
// horizon at the rim. Used by the direction plot and the sky map.
func polarXY(az, el float64) (float64, float64) {
	return polarAt(az, polarRimRadius*(90-clamp(el, 0, 90))/90)
}

// polarRadius is the per-variant elevation → radius mapping (see the file
// comment): az/el grows outward from the horizon at the centre, the sky view
// shrinks toward the zenith at the centre.
func polarRadius(variant string, el float64) float64 {
	el = clamp(el, 0, 90)
	if variant == "azel" {
		return polarRimRadius * el / 90
	}
	return polarRimRadius * (90 - el) / 90
}

// polarPoint maps azimuth/elevation to coordinates for the given variant.
func polarPoint(variant string, az, el float64) (float64, float64) {
	return polarAt(az, polarRadius(variant, el))
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

	// Elevation rings with labels. Sky view: horizon rim plus 30°/60°.
	// Az/el: RN2's matplotlib defaults for rlim(0, 92) — 20/40/60/80 and the
	// zenith rim.
	rings := []float64{0, 30, 60}
	if variant == "azel" {
		rings = []float64{20, 40, 60, 80, 90}
	}
	for _, el := range rings {
		r := polarRadius(variant, el)
		fmt.Fprintf(&b, `<circle cx="%v" cy="%v" r="%.1f" fill="none" stroke="%s" stroke-width="1"/>`, c, c, r, polarColors.grid)
		if el == 90 {
			continue // the zenith rim stays unlabeled so it doesn't collide with the 0° spoke label
		}
		fmt.Fprintf(&b, `<text x="%v" y="%.1f" font-size="10" fill="%s" text-anchor="middle">%.0f°</text>`, c, c-r-3, polarColors.ink, el)
	}
	// Angular spokes: compass points on the direction plot, plain degrees on
	// the az/el plot (matplotlib's default polar ticks).
	spokes := []struct {
		label string
		az    float64
	}{{"N", 0}, {"NE", 45}, {"E", 90}, {"SE", 135}, {"S", 180}, {"SW", 225}, {"W", 270}, {"NW", 315}}
	if variant == "azel" {
		for i := range spokes {
			spokes[i].label = fmt.Sprintf("%.0f°", spokes[i].az)
		}
	}
	for _, p := range spokes {
		x1, y1 := polarAt(p.az, 0)
		x2, y2 := polarAt(p.az, polarRimRadius)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="0.5"/>`, x1, y1, x2, y2, polarColors.grid)
		lx, ly := polarAt(p.az, polarRimRadius+16) // just outside the rim
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="%s" text-anchor="middle" dominant-baseline="middle">%s</text>`, lx, ly, polarColors.ink, p.label)
	}

	// The track.
	var pts strings.Builder
	for i, s := range above {
		x, y := polarPoint(variant, s.Azimuth, s.Elevation)
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", x, y)
	}
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2"/>`, pts.String(), polarColors.track)

	// Markers: start, end, max elevation.
	sx, sy := polarPoint(variant, above[0].Azimuth, above[0].Elevation)
	ex, ey := polarPoint(variant, above[len(above)-1].Azimuth, above[len(above)-1].Elevation)
	mx, my := polarPoint(variant, above[maxIdx].Azimuth, above[maxIdx].Elevation)
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
		sx, sy = polarPoint(variant, aos.Azimuth, aos.Elevation)
		ex, ey = polarPoint(variant, los.Azimuth, los.Elevation)
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
