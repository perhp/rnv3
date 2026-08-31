// Package predict computes satellite passes over an observer using SGP4.
// Replaces RN2's scripts/tools/pass_predict.py (pyephem).
package predict

import (
	"fmt"
	"math"
	"time"

	sgp4 "github.com/joshuaferrara/go-satellite"

	"github.com/perhp/rnv3/internal/tle"
)

// Observer is the ground station position.
type Observer struct {
	Lat       float64 // degrees, north positive
	Lon       float64 // degrees, east positive
	AltMeters float64
}

// Pass is one predicted horizon-to-horizon pass.
type Pass struct {
	AOS          time.Time
	LOS          time.Time
	MaxElevation float64 // degrees
	AOSAzimuth   float64 // degrees
	MaxAzimuth   float64 // degrees at max elevation
	LOSAzimuth   float64
	Northbound   bool // sub-satellite point moving north during the pass
}

func (p Pass) Duration() time.Duration { return p.LOS.Sub(p.AOS) }

// Direction returns the RN2-compatible direction string.
func (p Pass) Direction() string {
	if p.Northbound {
		return "northbound"
	}
	return "southbound"
}

const (
	coarseStep = 20 * time.Second
	fineStep   = time.Second
)

// Passes returns every pass of the satellite above the horizon between from
// and to, in chronological order. TLE lines must already be validated
// (tle.ValidateLines) — the SGP4 library terminates the process on garbage.
func Passes(t tle.TLE, obs Observer, from, to time.Time) ([]Pass, error) {
	if err := tle.ValidateLines(t.Line1, t.Line2, t.NoradID); err != nil {
		return nil, fmt.Errorf("refusing to propagate invalid TLE for %d: %w", t.NoradID, err)
	}
	sat := sgp4.TLEToSat(t.Line1, t.Line2, sgp4.GravityWGS84)

	var passes []Pass
	prevUp := false
	cursor := from
	for cursor.Before(to) {
		el, _ := lookAngles(sat, obs, cursor)
		up := el > 0
		if up && !prevUp {
			aos := refineCrossing(sat, obs, cursor.Add(-coarseStep), cursor, true)
			p, losFound := tracePass(sat, obs, aos, to)
			if !losFound {
				break // pass runs past the window edge; drop the partial pass
			}
			passes = append(passes, p)
			cursor = p.LOS.Add(coarseStep)
			prevUp = false
			continue
		}
		prevUp = up
		cursor = cursor.Add(coarseStep)
	}
	return passes, nil
}

// tracePass walks a pass from its AOS at fine resolution, recording azimuths
// and the elevation maximum, until the satellite sets or the limit is hit.
func tracePass(sat sgp4.Satellite, obs Observer, aos time.Time, limit time.Time) (Pass, bool) {
	p := Pass{AOS: aos}
	_, p.AOSAzimuth = lookAngles(sat, obs, aos)

	latStart := subLatitude(sat, aos)
	for cur := aos; ; cur = cur.Add(fineStep) {
		if cur.After(limit.Add(30 * time.Minute)) {
			return p, false // never sets within a sane horizon — treat as no pass
		}
		el, az := lookAngles(sat, obs, cur)
		if el > p.MaxElevation {
			p.MaxElevation = el
			p.MaxAzimuth = az
		}
		if el <= 0 && cur.After(aos) {
			p.LOS = cur
			p.LOSAzimuth = az
			p.Northbound = subLatitude(sat, cur) > latStart
			return p, p.LOS.Before(limit)
		}
	}
}

// refineCrossing bisects [below, above] down to 1s for the horizon crossing.
func refineCrossing(sat sgp4.Satellite, obs Observer, below, above time.Time, rising bool) time.Time {
	for above.Sub(below) > fineStep {
		mid := below.Add(above.Sub(below) / 2)
		el, _ := lookAngles(sat, obs, mid)
		if (el > 0) == rising {
			above = mid
		} else {
			below = mid
		}
	}
	return above
}

// Sample is one look-angle observation on a pass track.
type Sample struct {
	Time      time.Time
	Azimuth   float64 // degrees
	Elevation float64 // degrees
}

// Track samples the satellite's look angles from the observer over [from, to]
// at the given step — the data source for polar pass plots.
func Track(t tle.TLE, obs Observer, from, to time.Time, step time.Duration) ([]Sample, error) {
	if err := tle.ValidateLines(t.Line1, t.Line2, t.NoradID); err != nil {
		return nil, fmt.Errorf("refusing to propagate invalid TLE for %d: %w", t.NoradID, err)
	}
	if step <= 0 {
		step = time.Second
	}
	sat := sgp4.TLEToSat(t.Line1, t.Line2, sgp4.GravityWGS84)
	var out []Sample
	for cur := from; !cur.After(to); cur = cur.Add(step) {
		el, az := lookAngles(sat, obs, cur)
		out = append(out, Sample{Time: cur, Azimuth: az, Elevation: el})
	}
	return out, nil
}

// lookAngles returns elevation and azimuth in degrees at time tt.
func lookAngles(sat sgp4.Satellite, obs Observer, tt time.Time) (elevation, azimuth float64) {
	u := tt.UTC()
	pos, _ := sgp4.Propagate(sat, u.Year(), int(u.Month()), u.Day(), u.Hour(), u.Minute(), u.Second())
	jday := sgp4.JDay(u.Year(), int(u.Month()), u.Day(), u.Hour(), u.Minute(), u.Second())
	la := sgp4.ECIToLookAngles(pos, sgp4.LatLong{
		Latitude:  obs.Lat * sgp4.DEG2RAD,
		Longitude: obs.Lon * sgp4.DEG2RAD,
	}, obs.AltMeters/1000.0, jday)
	return la.El * sgp4.RAD2DEG, la.Az * sgp4.RAD2DEG
}

// subLatitude returns the sub-satellite latitude in degrees at time tt.
func subLatitude(sat sgp4.Satellite, tt time.Time) float64 {
	u := tt.UTC()
	pos, _ := sgp4.Propagate(sat, u.Year(), int(u.Month()), u.Day(), u.Hour(), u.Minute(), u.Second())
	gmst := sgp4.GSTimeFromDate(u.Year(), int(u.Month()), u.Day(), u.Hour(), u.Minute(), u.Second())
	_, _, ll := sgp4.ECIToLLA(pos, gmst)
	lat := ll.Latitude * sgp4.RAD2DEG
	if math.IsNaN(lat) {
		return 0
	}
	return lat
}
