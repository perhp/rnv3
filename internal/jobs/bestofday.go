package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/process"
	"github.com/perhp/rnv3/internal/store"
)

// Representative enhancements for the best-of-day post, in preference order
// (RN2 best_of_day.sh candidates).
var (
	noaaDayKinds   = []string{"MSA", "MSA-precip", "HVC", "MCIR"}
	noaaNightKinds = []string{"MCIR", "MCIR-precip", "therm", "HVCT"}
	meteorKinds    = []string{"MSA_corrected", "Natural_Color_corrected", "321_corrected", "221_corrected",
		"MCIR_corrected", "Night_Microphysics_corrected", "654_corrected"}
	auxKinds = map[string]bool{"polar-azel": true, "polar-direction": true, "website-thumbnail": true,
		"spectrogram": true, "pristine": true, "histogram": true}
)

// bestOfDay is RN2's best_of_day.sh: once a day, rebuild the daily
// artifacts as a fallback, pick the strongest capture, and push one summary
// with the day's mosaics and timelapses attached.
func (j *Jobs) bestOfDay(ctx context.Context, now time.Time) {
	cfg := j.Prov.Get()
	if !cfg.Daily.BestOfDayPush && !cfg.Daily.Timelapse.Enabled && !cfg.Daily.Mosaic.Enabled {
		return
	}
	log := logger()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	annotation := "Daily summary " + dayStart.Format("2006-01-02")
	if cfg.Station.Location != "" {
		annotation += " - " + cfg.Station.Location
	}
	var files []string

	if cfg.Daily.BestOfDayPush {
		best, err := j.St.BestCaptureOfDay(dayStart, dayStart.AddDate(0, 0, 1))
		switch {
		case err != nil:
			log.Warn("best of day: query failed", "err", err)
		case best == nil:
			log.Info("best of day: no captures today - nothing to pick")
		default:
			if img := j.representativeImage(cfg, best); img != "" {
				annotation += fmt.Sprintf(" | Best capture: %s, max elev %.0f°", best.Satellite, best.MaxElevation)
				if best.MaxSNR != nil {
					annotation += fmt.Sprintf(", peak SNR %.1f dB", *best.MaxSNR)
				}
				files = append(files, img)
				log.Info("best of day", "image", filepath.Base(img), "satellite", best.Satellite)
			} else {
				log.Warn("best of day: no image on disk for capture", "pass_id", best.ID)
			}
		}
	}

	if cfg.Daily.Timelapse.Enabled || cfg.Daily.Mosaic.Enabled {
		// Rebuilt after every capture; this run catches a day whose last
		// capture finished without one. It happens whether or not the
		// summary is pushed.
		if err := process.BuildDailyArtifacts(cfg, j.St, now); err != nil {
			log.Warn("best of day: daily artifacts rebuild failed", "err", err)
		}
		if !cfg.Daily.BestOfDayPush {
			return
		}
		stamp := dayStart.Format("20060102")
		timelapses, _ := filepath.Glob(filepath.Join(cfg.Paths.Images, "timelapse-"+stamp+"-*.gif"))
		mosaics, _ := filepath.Glob(filepath.Join(cfg.Paths.Images, "mosaic-"+stamp+"-*.jpg"))
		sort.Strings(timelapses)
		sort.Strings(mosaics)
		if len(timelapses)+len(mosaics) > 0 {
			files = append(files, timelapses...)
			files = append(files, mosaics...)
			annotation += fmt.Sprintf(" | %d timelapse(s), %d mosaic(s)", len(timelapses), len(mosaics))
		}
	}

	if len(files) == 0 {
		log.Info("best of day: nothing to push")
		return
	}
	if j.Notify != nil {
		j.Notify.DailySummary(ctx, annotation, files)
	}
	log.Info("best of day push complete", "files", len(files))
}

// representativeImage picks the capture's lead enhancement by satellite
// type and daylight, falling back to its first non-graph image. Only files
// actually on disk qualify — a stale row (pruned or deleted by hand) must
// not turn the summary into a missing attachment.
func (j *Jobs) representativeImage(cfg *config.Config, p *store.SchedulePass) string {
	images, err := j.St.ImagesForPass(p.ID)
	if err != nil || len(images) == 0 {
		return ""
	}
	exists := func(name string) (string, bool) {
		full := filepath.Join(cfg.Paths.Images, name)
		if st, err := os.Stat(full); err == nil && !st.IsDir() {
			return full, true
		}
		return "", false
	}
	byKind := map[string]string{}
	var first string
	for _, im := range images {
		if im.Path == "" || auxKinds[im.Kind] {
			continue
		}
		full, ok := exists(im.Path)
		if !ok {
			continue
		}
		byKind[im.Kind] = full
		if first == "" {
			first = full
		}
	}
	kinds := meteorKinds
	if isNOAA(cfg, p.Satellite) {
		kinds = noaaNightKinds
		if p.Daylight {
			kinds = noaaDayKinds
		}
	}
	for _, k := range kinds {
		if path, ok := byKind[k]; ok {
			return path
		}
	}
	return first
}

func isNOAA(cfg *config.Config, name string) bool {
	if sat, ok := cfg.SatelliteByName(name); ok {
		return sat.Type == config.SatNOAAAPT
	}
	return strings.HasPrefix(strings.ToUpper(name), "NOAA")
}
