package process

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// meteorStripPrefixes are tried in order; each strips only a leading match
// and the result feeds the next (RN2 `${var#prefix}` chain).
var meteorStripPrefixes = []string{
	"msu_mr_rgb_",
	"rgb_msu_mr_rgb_",
	"rgb_msu_mr_",
	"msu_mr_",
}

// meteorWebsiteThumbKinds is RN2's gallery-card preference order, verbatim.
var meteorWebsiteThumbKinds = []string{
	"MSA_corrected", "MSA_projected",
	"Natural_Color_corrected", "Natural_Color_projected",
	"MCIR_corrected", "MCIR_projected",
	"321_corrected", "321_projected",
	"221_corrected", "221_projected",
	"Day_Microphysics_corrected", "Night_Microphysics_corrected",
	"124_corrected", "456_corrected", "654_corrected",
	"39um_Shortwave_IR_corrected", "39um_Shortwave_IR_Calibrated_corrected",
	"Thermal_Channel_corrected",
}

// processMeteor ports receive_meteor.sh's post-decode image handling:
//  1. copy `*_corrected.png`/`*_projected.png` from MSU-MR/ into
//     "MSU-MR (Filled)/" WITHOUT clobbering (gap-filled versions win)
//  2. keep only *projected*, *corrected* and raw MSU-MR-* channel images
//  3. drop rgb projected composites whose corrected counterpart is missing
//     (channels not broadcast) and all *_equirect_corrected.png duplicates
//  4. `*_map.png` → `.png`
//  5. Northbound flip (when configured) for corrected + raw channel images
//     only — projected images are map-anchored and never flipped
//  6. prefix stripping, JPEG normalize + thumbnail
func processMeteor(workDir, imagesDir, thumbsDir, fileBase string, northbound, flipEnabled bool, quality int) ([]Produced, error) {
	filled := filepath.Join(workDir, "MSU-MR (Filled)")
	plain := filepath.Join(workDir, "MSU-MR")
	if err := os.MkdirAll(filled, 0o755); err != nil {
		return nil, err
	}

	// 1. no-clobber copy from MSU-MR.
	if entries, err := os.ReadDir(plain); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if !strings.HasSuffix(n, "_corrected.png") && !strings.HasSuffix(n, "_projected.png") {
				continue
			}
			dst := filepath.Join(filled, n)
			if _, err := os.Stat(dst); err == nil {
				continue // gap-filled version already there — never overwrite
			}
			if err := copyFile(filepath.Join(plain, n), dst); err != nil {
				slog.Warn("cannot copy meteor product", "file", n, "err", err)
			}
		}
	}

	// 2. prune everything that is not a composite or raw channel.
	names, err := listPNGs(filled)
	if err != nil {
		return nil, err
	}
	kept := names[:0]
	for _, n := range names {
		if strings.Contains(n, "projected") || strings.Contains(n, "corrected") || strings.HasPrefix(n, "MSU-MR-") {
			kept = append(kept, n)
		} else {
			os.Remove(filepath.Join(filled, n))
		}
	}
	names = kept

	// 3. rgb projected composites without a corrected counterpart, and
	// equirect corrected duplicates.
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	kept = names[:0]
	for _, n := range names {
		if strings.HasPrefix(n, "rgb_msu_mr_rgb_") && strings.HasSuffix(n, "_projected.png") {
			corrected := strings.Replace(n, "rgb_msu_mr_rgb_", "msu_mr_rgb_", 1)
			corrected = strings.Replace(corrected, "_projected.png", "_corrected.png", 1)
			if !nameSet[corrected] {
				os.Remove(filepath.Join(filled, n))
				continue
			}
		}
		if strings.HasSuffix(n, "_equirect_corrected.png") {
			os.Remove(filepath.Join(filled, n))
			continue
		}
		kept = append(kept, n)
	}

	// 4. map-overlay variants become canonical.
	names = renameMapVariants(filled, kept)

	// 5+6. flip, strip, normalize.
	var produced []Produced
	for _, name := range names {
		kind := name
		for _, p := range meteorStripPrefixes {
			kind = strings.TrimPrefix(kind, p)
		}
		kind = strings.TrimSuffix(kind, ".png")
		if kind == "" {
			slog.Warn("meteor product name stripped to nothing, skipping", "file", name)
			continue
		}

		img, err := loadPNG(filepath.Join(filled, name))
		if err != nil {
			slog.Warn("cannot decode meteor product", "file", name, "err", err)
			continue
		}
		flip := northbound && flipEnabled &&
			(strings.HasSuffix(name, "_corrected.png") || strings.HasPrefix(name, "MSU-MR-"))

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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
