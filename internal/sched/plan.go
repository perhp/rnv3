// Package sched builds the pass plan (prediction + gates + overlap
// resolution) and runs the in-process scheduler that replaces cron + at.
package sched

import (
	"fmt"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/predict"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

// Candidate is one predicted pass with its planning outcome.
type Candidate struct {
	Sat        config.Satellite
	Pass       predict.Pass
	Skipped    bool
	SkipReason string
}

// BuildPlan predicts passes for every enabled satellite across the scheduling
// window, applies the elevation and sun gates, and resolves overlaps. The
// returned candidates are sorted by AOS and include skipped passes with their
// reason (so the UI can show conflicts, unlike RN2 which deleted the loser).
func BuildPlan(cfg *config.Config, tles tle.Set, now time.Time) ([]Candidate, error) {
	obs := predict.Observer{
		Lat:       cfg.Station.Latitude,
		Lon:       cfg.Station.Longitude,
		AltMeters: cfg.Station.Altitude,
	}
	horizon := now.Add(time.Duration(cfg.Scheduling.DaysAhead) * 24 * time.Hour)

	var candidates []*Candidate
	for _, sat := range cfg.EnabledSatellites() {
		t, ok := tles[sat.NoradID]
		if !ok {
			return nil, fmt.Errorf("no TLE for %s (NORAD %d)", sat.Name, sat.NoradID)
		}
		passes, err := predict.Passes(t, obs, now, horizon)
		if err != nil {
			return nil, fmt.Errorf("predict %s: %w", sat.Name, err)
		}
		for _, p := range passes {
			c := &Candidate{Sat: sat, Pass: p}
			// Elevation gate (strict >, RN2 parity).
			if !(p.MaxElevation > sat.MinElevation) {
				continue // below threshold passes were never rows in RN2 either
			}
			// Scheduling-time sun gate (Meteor visible-light passes).
			if sat.ScheduleSunMinElevation != nil {
				sunEl := predict.SunElevation(obs.Lat, obs.Lon, p.AOS)
				if sunEl < *sat.ScheduleSunMinElevation {
					c.Skipped = true
					c.SkipReason = fmt.Sprintf("sun at %.0f° below scheduling threshold %.0f°", sunEl, *sat.ScheduleSunMinElevation)
				}
			}
			candidates = append(candidates, c)
		}
	}

	sortByAOS(candidates)
	if cfg.Scheduling.ResolveOverlaps {
		resolveOverlaps(candidates, cfg.Scheduling.PreferMeteorOverNOAA)
	}

	out := make([]Candidate, len(candidates))
	for i, c := range candidates {
		out[i] = *c
	}
	return out, nil
}

// resolveOverlaps accepts passes strongest-first (Meteor preference when
// enabled, then max elevation) and skips any weaker pass that conflicts with
// an accepted one. Priority-order greedy handles overlap chains correctly: in
// A–B–C where only B conflicts with both, evicting B keeps A *and* C — a case
// RN2's adjacent-pair comparison (and a naive AOS-order walk) gets wrong.
func resolveOverlaps(cands []*Candidate, preferMeteor bool) {
	byPriority := make([]*Candidate, 0, len(cands))
	for _, c := range cands {
		if !c.Skipped {
			byPriority = append(byPriority, c)
		}
	}
	for i := 1; i < len(byPriority); i++ {
		for j := i; j > 0 && beats(byPriority[j], byPriority[j-1], preferMeteor); j-- {
			byPriority[j-1], byPriority[j] = byPriority[j], byPriority[j-1]
		}
	}

	var accepted []*Candidate
	for _, c := range byPriority {
		conflict := (*Candidate)(nil)
		for _, a := range accepted {
			if overlaps(c, a) {
				conflict = a
				break
			}
		}
		if conflict == nil {
			accepted = append(accepted, c)
		} else {
			c.Skipped = true
			c.SkipReason = overlapReason(conflict)
		}
	}
}

func overlaps(a, b *Candidate) bool {
	return a.Pass.AOS.Before(b.Pass.LOS) && b.Pass.AOS.Before(a.Pass.LOS)
}

// beats decides whether a outranks b: Meteor preference first (when enabled
// and exactly one side is Meteor), then higher max elevation.
func beats(a, b *Candidate, preferMeteor bool) bool {
	if preferMeteor {
		am := a.Sat.Type == config.SatMeteorLRPT
		bm := b.Sat.Type == config.SatMeteorLRPT
		if am != bm {
			return am
		}
	}
	return a.Pass.MaxElevation > b.Pass.MaxElevation
}

func overlapReason(winner *Candidate) string {
	return fmt.Sprintf("overlaps %s pass at %s (%.0f°)",
		winner.Sat.Name, winner.Pass.AOS.UTC().Format("2006-01-02 15:04Z"), winner.Pass.MaxElevation)
}

func sortByAOS(cands []*Candidate) {
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && cands[j-1].Pass.AOS.After(cands[j].Pass.AOS); j-- {
			cands[j-1], cands[j] = cands[j], cands[j-1]
		}
	}
}

// ToStorePasses converts candidates to store rows.
func ToStorePasses(cands []Candidate) []store.Pass {
	out := make([]store.Pass, 0, len(cands))
	for _, c := range cands {
		state := store.StateScheduled
		if c.Skipped {
			state = store.StateSkipped
		}
		out = append(out, store.Pass{
			Satellite:    c.Sat.Name,
			StartTS:      c.Pass.AOS.Unix(),
			EndTS:        c.Pass.LOS.Unix(),
			MaxElevation: c.Pass.MaxElevation,
			StartAzimuth: c.Pass.AOSAzimuth,
			AzimuthAtMax: c.Pass.MaxAzimuth,
			Direction:    c.Pass.Direction(),
			State:        state,
			ErrorText:    c.SkipReason,
		})
	}
	return out
}
