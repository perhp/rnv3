package predict

import (
	"testing"
	"time"
)

func TestSunElevationEquinoxNoonAtEquator(t *testing.T) {
	// Near the March 2026 equinox, solar noon at (0,0) is ~12:07 UTC and the
	// sun is close to the zenith.
	el := SunElevation(0, 0, time.Date(2026, 3, 20, 12, 7, 0, 0, time.UTC))
	if el < 85 {
		t.Errorf("equinox noon at equator: elevation %.1f°, want > 85°", el)
	}
}

func TestSunElevationEquinoxMidnightAtEquator(t *testing.T) {
	el := SunElevation(0, 0, time.Date(2026, 3, 20, 0, 7, 0, 0, time.UTC))
	if el > -80 {
		t.Errorf("equinox midnight at equator: elevation %.1f°, want < -80°", el)
	}
}

func TestSunElevationSummerSolsticeNYC(t *testing.T) {
	// Solar noon in New York on the June solstice (~16:57 UTC): elevation
	// should be about 90 - (40.7 - 23.44) ≈ 72.7°.
	el := SunElevation(40.7128, -74.0060, time.Date(2026, 6, 21, 16, 57, 0, 0, time.UTC))
	if el < 68 || el > 76 {
		t.Errorf("solstice noon NYC: elevation %.1f°, want ~72.7°", el)
	}
}

func TestSunElevationContinuity(t *testing.T) {
	// Elevation should change smoothly, never jumping more than ~0.3°/min.
	prev := SunElevation(56.0, 10.0, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC))
	for m := 1; m < 24*60; m++ {
		cur := SunElevation(56.0, 10.0, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC).Add(time.Duration(m)*time.Minute))
		if d := cur - prev; d > 0.35 || d < -0.35 {
			t.Fatalf("discontinuity at minute %d: %.2f° jump", m, d)
		}
		prev = cur
	}
}
