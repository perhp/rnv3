package capture

import (
	"regexp"
	"strconv"
)

// snrPattern matches SNR readings in SatDump's live output, tolerant of the
// varying separators between "SNR" and the value (same tolerance as RN2's
// extract_snr_stats grep).
var snrPattern = regexp.MustCompile(`SNR[^0-9,\-]{0,10}(-?[0-9]+(?:\.[0-9]+)?)`)

// ansiPattern strips terminal escape sequences SatDump emits for its
// progress display.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// SNRStats accumulates SNR readings from decoder output lines.
type SNRStats struct {
	count int
	sum   float64
	max   float64
}

// Feed scans one output line for SNR readings.
func (s *SNRStats) Feed(line string) {
	for _, m := range snrPattern.FindAllStringSubmatch(line, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		if s.count == 0 || v > s.max {
			s.max = v
		}
		s.count++
		s.sum += v
	}
}

// Result returns (max, avg, ok); ok is false when no readings were seen
// (analog-only output or a dead capture — stored as NULL, like RN2).
func (s *SNRStats) Result() (max, avg float64, ok bool) {
	if s.count == 0 {
		return 0, 0, false
	}
	return s.max, s.sum / float64(s.count), true
}

// CleanLine strips ANSI escapes and trims carriage-return artifacts from a
// decoder output line.
func CleanLine(line string) string {
	return ansiPattern.ReplaceAllString(line, "")
}
