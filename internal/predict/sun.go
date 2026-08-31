package predict

import (
	"math"
	"time"
)

// SunElevation returns the sun's elevation in degrees above the horizon for
// an observer at lat/lon (degrees, north/east positive) at time t.
//
// NOAA solar position algorithm (Meeus); accurate to well under 0.1°, far
// better than needed for day/night gating. Replaces RN2's sun.py (pyephem).
func SunElevation(lat, lon float64, t time.Time) float64 {
	const deg = math.Pi / 180

	jd := float64(t.UnixMilli())/86400000.0 + 2440587.5
	T := (jd - 2451545.0) / 36525.0 // julian centuries since J2000

	// Geometric mean longitude and anomaly of the sun (degrees).
	L0 := math.Mod(280.46646+T*(36000.76983+0.0003032*T), 360)
	M := 357.52911 + T*(35999.05029-0.0001537*T)
	e := 0.016708634 - T*(0.000042037+0.0000001267*T)

	// Equation of center, true and apparent longitude.
	C := math.Sin(M*deg)*(1.914602-T*(0.004817+0.000014*T)) +
		math.Sin(2*M*deg)*(0.019993-0.000101*T) +
		math.Sin(3*M*deg)*0.000289
	trueLong := L0 + C
	omega := 125.04 - 1934.136*T
	lambda := trueLong - 0.00569 - 0.00478*math.Sin(omega*deg)

	// Obliquity of the ecliptic (corrected) and solar declination.
	eps0 := 23.0 + (26.0+21.448/60.0)/60.0 - T*(46.8150+T*(0.00059-T*0.001813))/3600.0
	eps := eps0 + 0.00256*math.Cos(omega*deg)
	decl := math.Asin(math.Sin(eps*deg) * math.Sin(lambda*deg))

	// Equation of time in minutes.
	y := math.Tan(eps * deg / 2)
	y *= y
	eqTime := 4 / deg * (y*math.Sin(2*L0*deg) -
		2*e*math.Sin(M*deg) +
		4*e*y*math.Sin(M*deg)*math.Cos(2*L0*deg) -
		0.5*y*y*math.Sin(4*L0*deg) -
		1.25*e*e*math.Sin(2*M*deg))

	// True solar time → hour angle.
	utc := t.UTC()
	minutes := float64(utc.Hour())*60 + float64(utc.Minute()) + float64(utc.Second())/60
	trueSolarTime := math.Mod(minutes+eqTime+4*lon+1440, 1440)
	hourAngle := trueSolarTime/4 - 180
	if hourAngle < -180 {
		hourAngle += 360
	}

	sinEl := math.Sin(lat*deg)*math.Sin(decl) +
		math.Cos(lat*deg)*math.Cos(decl)*math.Cos(hourAngle*deg)
	return math.Asin(sinEl) / deg
}
