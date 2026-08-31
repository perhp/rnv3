package capture

import (
	"os"
	"strconv"
	"strings"
)

// availableMemoryMB reads MemAvailable from /proc/meminfo. Returns 0 when it
// cannot be determined (non-Linux, parse failure), which routes captures to
// disk instead of ramfs — the safe default (RN2's Meteor script defaulted the
// same way; its NOAA script did not and could crash on unparsable output).
func availableMemoryMB() int {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
