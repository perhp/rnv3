package process

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Produced is one finished gallery image.
type Produced struct {
	Kind      string // enhancement name, e.g. "MCIR", "MSA_corrected"
	ImageName string // filename in the images dir
	ThumbName string // filename in the thumbs dir
}

// noaaStrip lists the substrings removed (globally, RN2 `${var//x}`) from
// SatDump's NOAA APT output names. Order matters: longer prefixes first.
var noaaStrip = []string{
	"rgb_avhrr_3_rgb_",
	"avhrr_apt_rgb_",
	"avhrr_3_rgb_",
	"avhrr_apt_",
	"_enhancement",
	"_(channel_1)",
	"_(channel_4)",
}

// processNOAA ports receive_noaa.sh's post-decode image handling:
//  1. `*_map.png` → `.png` (the map-overlay variant becomes canonical,
//     overwriting the plain one)
//  2. Northbound flip for everything EXCEPT `rgb_*` composites (RN2
//     pre-flipped those once so the general flip restored them — SatDump
//     emits them already north-up)
//  3. `_(Uncalibrated)` variants: dropped when the calibrated file exists,
//     otherwise promoted to the calibrated name
//  4. prefix/marker stripping, then JPEG normalize + 300px thumbnail
func processNOAA(workDir, imagesDir, thumbsDir, fileBase string, northbound bool, quality int) ([]Produced, error) {
	names, err := listPNGs(workDir)
	if err != nil {
		return nil, err
	}

	names = renameMapVariants(workDir, names)

	// Uncalibrated dedup on the current file set.
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}

	var produced []Produced
	for _, name := range names {
		src := name
		if strings.Contains(name, "_(Uncalibrated)") {
			calibrated := strings.ReplaceAll(name, "_(Uncalibrated)", "")
			if nameSet[calibrated] {
				os.Remove(filepath.Join(workDir, name))
				continue // calibrated version wins
			}
			src = calibrated // promote: process under the calibrated name
		}

		kind := src
		for _, s := range noaaStrip {
			kind = strings.ReplaceAll(kind, s, "")
		}
		kind = strings.TrimSuffix(kind, ".png")
		if kind == "" {
			slog.Warn("NOAA product name stripped to nothing, skipping", "file", name)
			continue
		}

		img, err := loadPNG(filepath.Join(workDir, name))
		if err != nil {
			slog.Warn("cannot decode NOAA product", "file", name, "err", err)
			continue
		}
		// rgb_* composites are emitted north-up by SatDump; everything else
		// needs the Northbound flip.
		flip := northbound && !strings.HasPrefix(name, "rgb_")

		imageName := fileBase + "-" + kind + ".jpg"
		if err := normalize(img, flip,
			filepath.Join(imagesDir, imageName),
			filepath.Join(thumbsDir, imageName), quality); err != nil {
			return produced, err
		}
		produced = append(produced, Produced{Kind: kind, ImageName: imageName, ThumbName: imageName})
	}
	return produced, nil
}

// noaaWebsiteThumbKinds is the representative-image preference for the
// gallery card, by day/night (derived from RN2's best_of_day candidates —
// RN2 itself never created a NOAA website thumbnail, which left broken
// gallery cards; rnv3 fixes that).
var noaaWebsiteThumbKinds = map[bool][]string{
	true:  {"MSA", "MSA-precip", "HVC", "MCIR"},     // daylight
	false: {"MCIR", "MCIR-precip", "therm", "HVCT"}, // night
}

// listPNGs returns the .png filenames in dir's top level, sorted.
func listPNGs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".png") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// renameMapVariants makes `*_map.png` files canonical by renaming them over
// their plain counterparts, returning the updated name list.
func renameMapVariants(dir string, names []string) []string {
	result := map[string]bool{}
	for _, n := range names {
		result[n] = true
	}
	for _, n := range names {
		if !strings.HasSuffix(n, "_map.png") {
			continue
		}
		target := strings.TrimSuffix(n, "_map.png") + ".png"
		os.Remove(filepath.Join(dir, target)) // os.Rename won't clobber on Windows
		if err := os.Rename(filepath.Join(dir, n), filepath.Join(dir, target)); err != nil {
			slog.Warn("cannot rename map variant", "file", n, "err", err)
			continue
		}
		delete(result, n)
		result[target] = true
	}
	out := make([]string, 0, len(result))
	for n := range result {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
