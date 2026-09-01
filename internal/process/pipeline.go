package process

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/predict"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

// Pipeline is the real PostProcessor: SatDump work-dir products → gallery
// imagery + DB rows + polar plots + sky map + daily artifacts.
type Pipeline struct {
	Prov *config.Provider
	St   *store.Store
	TLEs *tle.Manager
}

// Process implements capture.PostProcessor. Returns the satellite images
// produced, as absolute paths in production order (auxiliary artifacts like
// polar plots are not included: they don't count toward the decoded/failed
// decision and are not pushed).
func (pl *Pipeline) Process(ctx context.Context, p store.Pass, sat config.Satellite, workDir, fileBase string, daylight bool) ([]string, error) {
	cfg := pl.Prov.Get()
	northbound := p.Direction == "northbound"

	var produced []Produced
	var err error
	switch sat.Type {
	case config.SatNOAAAPT:
		produced, err = processNOAA(workDir, cfg.Paths.Images, cfg.Paths.Thumbs, fileBase,
			northbound, cfg.Processing.NOAA.JPGQuality)
	case config.SatMeteorLRPT:
		produced, err = processMeteor(workDir, cfg.Paths.Images, cfg.Paths.Thumbs, fileBase,
			northbound, cfg.Processing.Meteor.FlipNorthbound, cfg.Processing.Meteor.JPGQuality)
	}
	if err != nil {
		// A half-written product set must not become a capture: discard
		// what was produced so nothing orphaned lingers in the gallery dirs.
		for _, pr := range produced {
			os.Remove(filepath.Join(cfg.Paths.Images, pr.ImageName))
			os.Remove(filepath.Join(cfg.Paths.Thumbs, pr.ThumbName))
		}
		return nil, err
	}
	paths := make([]string, 0, len(produced))
	for _, pr := range produced {
		paths = append(paths, filepath.Join(cfg.Paths.Images, pr.ImageName))
	}
	if len(produced) == 0 {
		return nil, nil // failed pass: no aux artifacts (RN2 parity)
	}

	for _, pr := range produced {
		if err := pl.St.AddImage(p.ID, pr.Kind, pr.ImageName, pr.ThumbName); err != nil {
			slog.Error("cannot register image", "kind", pr.Kind, "err", err)
		}
	}

	pl.websiteThumbnail(cfg, p, sat, produced, fileBase, daylight)
	pl.polarPlots(cfg, p, sat, fileBase)

	return paths, nil
}

// UpdateAggregates rebuilds the station-wide artifacts (sky map, daily
// mosaics/timelapses). The runner calls it AFTER the pass reaches its
// terminal DB state, so the current pass — decoded or failed — is included
// (RN2 likewise ran sky_quality_map.sh and daily_imagery.sh unconditionally
// at the end of every capture).
func (pl *Pipeline) UpdateAggregates(ctx context.Context, passStart time.Time) {
	cfg := pl.Prov.Get()
	if points, err := pl.St.SkymapPoints(); err == nil {
		if err := WriteSkymap(points, cfg.Paths.Images); err != nil {
			slog.Warn("sky map update failed", "err", err)
		}
	} else {
		slog.Warn("cannot query sky map points", "err", err)
	}
	if err := BuildDailyArtifacts(cfg, pl.St, passStart); err != nil {
		slog.Warn("daily artifacts failed", "err", err)
	}
}

// websiteThumbnail picks the gallery-card image by preference order and
// copies its thumbnail — for BOTH satellite types (RN2 only did Meteor,
// leaving NOAA gallery cards broken).
func (pl *Pipeline) websiteThumbnail(cfg *config.Config, p store.Pass, sat config.Satellite, produced []Produced, fileBase string, daylight bool) {
	prefs := meteorWebsiteThumbKinds
	if sat.Type == config.SatNOAAAPT {
		prefs = noaaWebsiteThumbKinds[daylight]
	}
	byKind := map[string]Produced{}
	for _, pr := range produced {
		byKind[pr.Kind] = pr
	}
	chosen, ok := Produced{}, false
	for _, kind := range prefs {
		if pr, found := byKind[kind]; found {
			chosen, ok = pr, true
			break
		}
	}
	if !ok {
		chosen = produced[0] // fallback: first produced image (RN2 best_of_day behavior)
	}
	dst := filepath.Join(cfg.Paths.Thumbs, fileBase+"-website-thumbnail.jpg")
	if err := copyFile(filepath.Join(cfg.Paths.Thumbs, chosen.ThumbName), dst); err != nil {
		slog.Warn("cannot create website thumbnail", "err", err)
		return
	}
	if err := pl.St.AddImage(p.ID, "website-thumbnail", "", fileBase+"-website-thumbnail.jpg"); err != nil {
		slog.Error("cannot register website thumbnail", "err", err)
	}
}

// polarPlots renders the az/el and direction SVGs from the current TLE set.
func (pl *Pipeline) polarPlots(cfg *config.Config, p store.Pass, sat config.Satellite, fileBase string) {
	if !cfg.Processing.PolarAzEl && !cfg.Processing.PolarDirect {
		return
	}
	set, _, err := pl.TLEs.Load()
	if err != nil {
		slog.Warn("no TLEs for polar plots", "err", err)
		return
	}
	t, ok := set[sat.NoradID]
	if !ok {
		slog.Warn("no TLE for satellite, skipping polar plots", "norad_id", sat.NoradID)
		return
	}
	obs := predict.Observer{Lat: cfg.Station.Latitude, Lon: cfg.Station.Longitude, AltMeters: cfg.Station.Altitude}
	track, err := predict.Track(t, obs, time.Unix(p.StartTS, 0), time.Unix(p.EndTS, 0), time.Second)
	if err != nil {
		slog.Warn("cannot compute pass track", "err", err)
		return
	}

	if cfg.Processing.PolarAzEl {
		name := fileBase + "-polar-azel.svg"
		if err := PolarPlot("azel", sat.Name, track, p.Direction, sat.MinElevation,
			filepath.Join(cfg.Paths.Images, name)); err != nil {
			slog.Warn("polar az/el plot failed", "err", err)
		} else if err := pl.St.AddImage(p.ID, "polar-azel", name, ""); err != nil {
			slog.Error("cannot register polar plot", "err", err)
		}
	}
	if cfg.Processing.PolarDirect {
		name := fileBase + "-polar-direction.svg"
		if err := PolarPlot("direction", sat.Name, track, p.Direction, sat.MinElevation,
			filepath.Join(cfg.Paths.Images, name)); err != nil {
			slog.Warn("polar direction plot failed", "err", err)
		} else if err := pl.St.AddImage(p.ID, "polar-direction", name, ""); err != nil {
			slog.Error("cannot register polar plot", "err", err)
		}
	}
}
