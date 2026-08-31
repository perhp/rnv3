package predict

import (
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/tle"
)

// A NOAA-19-shaped orbit (99.19° inclination, 14.12 rev/day) with epoch
// 2026-08-31 12:00 UTC. RAAN/anomaly are synthetic, which only shifts pass
// timing — the structural properties below hold for any sun-synchronous LEO.
func noaaLikeTLE(t *testing.T) tle.TLE {
	t.Helper()
	l1 := "1 33591U 09005A   26243.50000000  .00000100  00000-0  60000-4 0  999"
	l2 := "2 33591  99.1900 100.0000 0014000 120.0000 240.0000 14.1200000012345"
	l1 += string(rune('0' + tle.Checksum(l1)))
	l2 += string(rune('0' + tle.Checksum(l2)))
	tl := tle.TLE{Name: "NOAA 19 (test)", NoradID: 33591, Line1: l1, Line2: l2}
	if err := tle.ValidateLines(tl.Line1, tl.Line2, 33591); err != nil {
		t.Fatalf("test TLE invalid: %v", err)
	}
	return tl
}

func TestPassesStructure(t *testing.T) {
	obs := Observer{Lat: 40.7128, Lon: -74.0060, AltMeters: 10}
	from := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	passes, err := Passes(noaaLikeTLE(t), obs, from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// A polar LEO at 14.1 rev/day is visible from 40°N several times a day.
	if len(passes) < 2 || len(passes) > 14 {
		t.Fatalf("got %d passes in 24h, expected 2..14", len(passes))
	}

	var prevLOS time.Time
	for i, p := range passes {
		if !p.AOS.Before(p.LOS) {
			t.Errorf("pass %d: AOS %v not before LOS %v", i, p.AOS, p.LOS)
		}
		if d := p.Duration(); d < 2*time.Minute || d > 25*time.Minute {
			t.Errorf("pass %d: implausible duration %v", i, d)
		}
		if p.MaxElevation <= 0 || p.MaxElevation > 90 {
			t.Errorf("pass %d: max elevation %.1f out of (0, 90]", i, p.MaxElevation)
		}
		for _, az := range []float64{p.AOSAzimuth, p.MaxAzimuth, p.LOSAzimuth} {
			if az < 0 || az >= 360 {
				t.Errorf("pass %d: azimuth %.1f out of [0, 360)", i, az)
			}
		}
		if p.AOS.Before(prevLOS) {
			t.Errorf("pass %d overlaps previous pass", i)
		}
		if p.Direction() != "northbound" && p.Direction() != "southbound" {
			t.Errorf("pass %d: bad direction %q", i, p.Direction())
		}
		prevLOS = p.LOS
	}
}

func TestPassesEmptyWindow(t *testing.T) {
	obs := Observer{Lat: 40.7128, Lon: -74.0060}
	from := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	passes, err := Passes(noaaLikeTLE(t), obs, from, from.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) > 1 {
		t.Errorf("1-minute window returned %d passes", len(passes))
	}
}

func TestPassesRejectsInvalidTLE(t *testing.T) {
	bad := noaaLikeTLE(t)
	bad.Line1 = bad.Line1[:68] + "X"
	if _, err := Passes(bad, Observer{}, time.Now(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("invalid TLE accepted — would have crashed the SGP4 library")
	}
}
