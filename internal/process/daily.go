package process

import (
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

// BuildDailyArtifacts rewrites the day's mosaics and timelapses with every
// decoded pass so far — port of RN2's daily_imagery.sh, minus the flock (the
// pipeline runs captures sequentially in-process). day is any time within
// the local day to build.
func BuildDailyArtifacts(cfg *config.Config, st *store.Store, day time.Time) error {
	if !cfg.Daily.Timelapse.Enabled && !cfg.Daily.Mosaic.Enabled {
		return nil
	}
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	// Next calendar date, not +24h: DST transitions make local days 23 or 25
	// hours long.
	dayEnd := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, day.Location())
	passes, err := st.DecodedPassesBetween(dayStart, dayEnd)
	if err != nil {
		return err
	}
	stamp := dayStart.Format("20060102")

	if cfg.Daily.Timelapse.Enabled {
		for _, suffix := range cfg.Daily.Timelapse.Suffixes {
			frames := collectFrames(cfg.Paths.Images, passes, suffix, 0, false)
			if len(frames) < 2 {
				continue // a 1-frame "animation" is noise (RN2 parity)
			}
			out := filepath.Join(cfg.Paths.Images, fmt.Sprintf("timelapse-%s-%s.gif", stamp, variantName(suffix)))
			if err := writeTimelapse(frames, out); err != nil {
				slog.Warn("timelapse failed", "suffix", suffix, "err", err)
			}
		}
	}

	if cfg.Daily.Mosaic.Enabled {
		for _, suffix := range cfg.Daily.Mosaic.Suffixes {
			frames := collectFrames(cfg.Paths.Images, passes, suffix,
				cfg.Daily.Mosaic.MinSNR, cfg.Daily.Mosaic.DaylightOnly)
			if len(frames) < 2 {
				continue // a 1-frame "mosaic" is just a misleading copy (RN2 parity)
			}
			out := filepath.Join(cfg.Paths.Images, fmt.Sprintf("mosaic-%s-%s.jpg", stamp, variantName(suffix)))
			if err := writeMosaic(frames, out, cfg.Processing.Meteor.JPGQuality); err != nil {
				slog.Warn("mosaic failed", "suffix", suffix, "err", err)
			}
		}
	}
	return nil
}

// collectFrames resolves the day's image paths for one filename suffix,
// applying the mosaic quality filters (minSNR 0 disables; SNR-less passes
// are always kept, RN2 parity).
func collectFrames(imagesDir string, passes []store.DecodedPass, suffix string, minSNR float64, daylightOnly bool) []string {
	var out []string
	for _, p := range passes {
		if p.FileBase == "" {
			continue
		}
		if daylightOnly && !p.Daylight {
			continue
		}
		if minSNR > 0 && p.MaxSNR != nil && *p.MaxSNR < minSNR {
			continue
		}
		path := filepath.Join(imagesDir, p.FileBase+suffix)
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

// variantName turns "-321_projected.jpg" into "321_projected" for the
// artifact filename (RN2 naming).
func variantName(suffix string) string {
	v := strings.TrimPrefix(suffix, "-")
	if i := strings.LastIndexByte(v, '.'); i > 0 {
		v = v[:i]
	}
	return v
}

// timelapseWidth matches RN2's `-resize 800x`.
const timelapseWidth = 800

// timelapseDelay is in GIF centiseconds (RN2 `-delay 80` = 0.8 s/frame).
const timelapseDelay = 80

func writeTimelapse(framePaths []string, outPath string) error {
	anim := &gif.GIF{LoopCount: 0}
	for _, p := range framePaths {
		img, err := loadImage(p)
		if err != nil {
			slog.Warn("skipping unreadable timelapse frame", "file", p, "err", err)
			continue
		}
		small := resizeToWidth(img, timelapseWidth)
		pal := image.NewPaletted(small.Bounds(), palette.Plan9)
		draw.FloydSteinberg.Draw(pal, small.Bounds(), small, image.Point{})
		anim.Image = append(anim.Image, pal)
		anim.Delay = append(anim.Delay, timelapseDelay)
	}
	if len(anim.Image) < 2 {
		return fmt.Errorf("fewer than 2 usable frames")
	}
	tmp := outPath + ".tmp.gif" // keep the extension: format sniffers care
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gif.EncodeAll(f, anim); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	os.Remove(outPath)
	return os.Rename(tmp, outPath)
}

// writeMosaic lighten-blends the day's frames over black. Works without any
// warping because SatDump's projection grids are fixed and station-anchored;
// frames with a mismatched size (config changed mid-day) are skipped.
func writeMosaic(framePaths []string, outPath string, quality int) error {
	var canvas *image.RGBA
	used := 0
	for _, p := range framePaths {
		img, err := loadImage(p)
		if err != nil {
			slog.Warn("skipping unreadable mosaic frame", "file", p, "err", err)
			continue
		}
		if canvas == nil {
			canvas = toRGBA(img) // first frame defines the grid
			used++
			continue
		}
		if !img.Bounds().Size().Eq(canvas.Bounds().Size()) {
			slog.Warn("skipping mosaic frame with mismatched size", "file", p)
			continue
		}
		lightenBlend(canvas, img)
		used++
	}
	if canvas == nil {
		return fmt.Errorf("no usable frames")
	}
	slog.Debug("mosaic built", "frames", used, "out", outPath)
	tmp := outPath + ".tmp.jpg"
	if err := saveJPEG(tmp, canvas, quality); err != nil {
		return err
	}
	os.Remove(outPath)
	return os.Rename(tmp, outPath)
}
