package sched

import (
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/predict"
)

func cand(name string, satType config.SatelliteType, start, end time.Time, maxElev float64) *Candidate {
	return &Candidate{
		Sat:  config.Satellite{Name: name, Type: satType},
		Pass: predict.Pass{AOS: start, LOS: end, MaxElevation: maxElev},
	}
}

var t0 = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func TestResolveOverlapsMeteorBeatsNOAA(t *testing.T) {
	noaa := cand("NOAA 19", config.SatNOAAAPT, at(0), at(15), 80)
	meteor := cand("METEOR-M2 3", config.SatMeteorLRPT, at(10), at(25), 35)
	resolveOverlaps([]*Candidate{noaa, meteor}, true)
	if !noaa.Skipped {
		t.Error("NOAA should lose to Meteor despite higher elevation when preference is on")
	}
	if meteor.Skipped {
		t.Error("Meteor should win")
	}
}

func TestResolveOverlapsElevationWinsWithoutPreference(t *testing.T) {
	noaa := cand("NOAA 19", config.SatNOAAAPT, at(0), at(15), 80)
	meteor := cand("METEOR-M2 3", config.SatMeteorLRPT, at(10), at(25), 35)
	resolveOverlaps([]*Candidate{noaa, meteor}, false)
	if noaa.Skipped || !meteor.Skipped {
		t.Error("without Meteor preference the higher-elevation pass should win")
	}
}

func TestResolveOverlapsSameTypeHigherElevationWins(t *testing.T) {
	a := cand("NOAA 18", config.SatNOAAAPT, at(0), at(15), 40)
	b := cand("NOAA 19", config.SatNOAAAPT, at(10), at(25), 60)
	resolveOverlaps([]*Candidate{a, b}, true)
	if !a.Skipped || b.Skipped {
		t.Errorf("expected NOAA 19 (60°) to win over NOAA 18 (40°): a.Skipped=%v b.Skipped=%v", a.Skipped, b.Skipped)
	}
	if a.SkipReason == "" {
		t.Error("loser should carry a skip reason for the UI")
	}
}

func TestResolveOverlapsNoOverlapKeepsBoth(t *testing.T) {
	a := cand("NOAA 18", config.SatNOAAAPT, at(0), at(15), 40)
	b := cand("NOAA 19", config.SatNOAAAPT, at(20), at(35), 60)
	resolveOverlaps([]*Candidate{a, b}, true)
	if a.Skipped || b.Skipped {
		t.Error("non-overlapping passes must both stay scheduled")
	}
}

// Three-way chain: A(0-15, 50°) overlaps B(10-25, 55°), B overlaps C(22-38, 70°),
// A does not overlap C. RN2's adjacent-pair logic could end up keeping only C;
// the correct outcome keeps A and C and skips B.
func TestResolveOverlapsThreeWayChain(t *testing.T) {
	a := cand("NOAA 15", config.SatNOAAAPT, at(0), at(15), 50)
	b := cand("NOAA 18", config.SatNOAAAPT, at(10), at(25), 55)
	c := cand("NOAA 19", config.SatNOAAAPT, at(22), at(38), 70)
	cands := []*Candidate{a, b, c}
	resolveOverlaps(cands, false)
	if a.Skipped {
		t.Error("A should stay: its only conflict B is evicted by C")
	}
	if !b.Skipped {
		t.Error("B should be skipped (loses to C)")
	}
	if c.Skipped {
		t.Error("C should stay")
	}
}

func TestResolveOverlapsEvictionRestoresNonConflicting(t *testing.T) {
	// B(10-25, 55°) is accepted first among conflicts; C(12-30, 80°) beats B.
	// After eviction, only C remains — A(0-11, 50°) overlaps B but not C...
	// but A comes first in AOS order and stays accepted throughout.
	a := cand("NOAA 15", config.SatNOAAAPT, at(0), at(11), 50)
	b := cand("NOAA 18", config.SatNOAAAPT, at(10), at(25), 55)
	c := cand("NOAA 19", config.SatNOAAAPT, at(12), at(30), 80)
	resolveOverlaps([]*Candidate{a, b, c}, false)
	if a.Skipped {
		t.Error("A should stay scheduled")
	}
	if !b.Skipped {
		t.Error("B should lose to C")
	}
	if c.Skipped {
		t.Error("C should win")
	}
}

func TestResolveOverlapsIgnoresAlreadySkipped(t *testing.T) {
	gated := cand("METEOR-M2 3", config.SatMeteorLRPT, at(0), at(15), 45)
	gated.Skipped = true
	gated.SkipReason = "sun gate"
	noaa := cand("NOAA 19", config.SatNOAAAPT, at(5), at(20), 30)
	resolveOverlaps([]*Candidate{gated, noaa}, true)
	if noaa.Skipped {
		t.Error("a sun-gated Meteor pass must not block an overlapping NOAA pass")
	}
	if gated.SkipReason != "sun gate" {
		t.Error("existing skip reason must be preserved")
	}
}
