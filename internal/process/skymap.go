package process

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/perhp/rnv3/internal/store"
)

// SkymapFilename is the station-wide artifact name (SVG successor of RN2's
// sky-quality-map.png).
const SkymapFilename = "sky-quality-map.svg"

// WriteSkymap renders every historical pass at its max-elevation position:
// decoded passes as dots colored by peak SNR (light → dark blue, normalized
// to the station's best), SNR-less decodes hollow, failures as red crosses.
// Port of RN2's sky_quality_map.py.
func WriteSkymap(points []store.SkymapPoint, imagesDir string) error {
	bestSNR := 0.0
	for _, p := range points {
		if p.MaxSNR != nil && *p.MaxSNR > bestSNR {
			bestSNR = *p.MaxSNR
		}
	}

	var b strings.Builder
	c := polarSize / 2
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %[1]v %[1]v" font-family="Cascadia Mono, ui-monospace, monospace">`, polarSize)
	fmt.Fprintf(&b, `<rect width="%[1]v" height="%[1]v" fill="%s"/>`, polarSize, polarColors.bg)
	for _, el := range []float64{0, 30, 60} {
		r := (polarSize/2 - 40) * (90 - el) / 90
		fmt.Fprintf(&b, `<circle cx="%v" cy="%v" r="%.1f" fill="none" stroke="%s" stroke-width="1"/>`, c, c, r, polarColors.grid)
		fmt.Fprintf(&b, `<text x="%v" y="%.1f" font-size="10" fill="%s" text-anchor="middle">%.0f°</text>`, c, c-r-3, polarColors.ink, el)
	}
	for _, p := range []struct {
		label string
		az    float64
	}{{"N", 0}, {"E", 90}, {"S", 180}, {"W", 270}} {
		lx, ly := polarXY(p.az, -6)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="%s" text-anchor="middle" dominant-baseline="middle">%s</text>`, lx, ly, polarColors.ink, p.label)
	}

	for _, p := range points {
		x, y := polarXY(p.AzimuthAtMax, p.MaxElevation)
		switch {
		case p.Failed:
			fmt.Fprintf(&b, `<path d="M %.1f %.1f m -4 -4 l 8 8 m 0 -8 l -8 8" stroke="%s" stroke-width="1.5" opacity="0.7"/>`, x, y, polarColors.end)
		case p.MaxSNR == nil:
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" fill="none" stroke="#4d6fa5" stroke-width="1.2" opacity="0.8"/>`, x, y)
		default:
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4.5" fill="%s" opacity="0.85"><title>SNR %.1f dB</title></circle>`,
				x, y, snrColor(*p.MaxSNR, bestSNR), *p.MaxSNR)
		}
	}

	fmt.Fprintf(&b, `<text x="10" y="16" font-size="12" fill="%s">Reception quality by sky position</text>`, polarColors.ink)
	fmt.Fprintf(&b, `<text x="10" y="30" font-size="10" fill="%s">%d passes · color = peak SNR · hollow = no SNR · ✕ = failed</text>`, polarColors.ink, len(points))
	b.WriteString(`</svg>`)

	return WriteFileAtomic(filepath.Join(imagesDir, SkymapFilename), []byte(b.String()))
}

// snrColor interpolates RN2's colormap (#b7cdec → #123f79) by SNR fraction.
func snrColor(snr, best float64) string {
	f := 0.0
	if best > 0 {
		f = clamp(snr/best, 0, 1)
	}
	lerp := func(a, b int) int { return a + int(math.Round(f*float64(b-a))) }
	return fmt.Sprintf("#%02x%02x%02x", lerp(0xb7, 0x12), lerp(0xcd, 0x3f), lerp(0xec, 0x79))
}
