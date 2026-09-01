package jobs

import (
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/perhp/rnv3/internal/process"
)

// workDirMaxAge: work directories kept after a failed capture (for the
// satdump.log) are swept after this long.
const workDirMaxAge = 7 * 24 * time.Hour

var dailyArtifact = regexp.MustCompile(`^(?:mosaic|timelapse)-(\d{8})`)

// prune applies retention: captures older than prune_images_older_than_days
// go entirely (images, thumbnails, rows — the setting RN2 shipped but never
// acted on), daily artifacts past the same age go too, and stale work
// directories are always swept.
func (j *Jobs) prune(now time.Time) {
	cfg := j.Prov.Get()
	log := logger()

	if days := cfg.Retention.PruneImagesOlderThanDays; days > 0 {
		cutoff := now.AddDate(0, 0, -days)
		ids, err := j.St.DecodedIDsBefore(cutoff)
		if err != nil {
			log.Warn("prune: cannot list old captures", "err", err)
		}
		removed := 0
		for _, id := range ids {
			if err := process.RemoveCapture(j.St, cfg.Paths.Images, cfg.Paths.Thumbs, id); err != nil {
				log.Warn("prune: cannot remove capture", "pass_id", id, "err", err)
				continue
			}
			removed++
		}
		artifacts := 0
		entries, _ := os.ReadDir(cfg.Paths.Images)
		for _, e := range entries {
			m := dailyArtifact.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			day, err := time.ParseInLocation("20060102", m[1], now.Location())
			if err != nil || !day.Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(cfg.Paths.Images, e.Name())) == nil {
				artifacts++
			}
		}
		if removed+artifacts > 0 {
			log.Info("prune: retention applied", "older_than_days", days, "captures_removed", removed, "daily_artifacts_removed", artifacts)
		}
	}

	for _, base := range []string{cfg.Paths.Work, filepath.Join(cfg.Paths.Ramfs, "work")} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < workDirMaxAge {
				continue
			}
			dir := filepath.Join(base, e.Name())
			if err := os.RemoveAll(dir); err == nil {
				log.Info("prune: removed stale work dir", "dir", dir)
			}
		}
	}
}
