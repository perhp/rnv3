// Package hostinfo reads the station host's health figures for the
// station.stats event: CPU temperature and usage, memory, disk, uptime.
package hostinfo

import "time"

// Stats are nullable per reading: a host that cannot provide one reports
// nil rather than a fake number.
type Stats struct {
	RecordedAt       time.Time
	CPUTemperatureC  *float64
	CPUUsagePercent  *float64
	MemoryTotalBytes *uint64
	MemoryUsedBytes  *uint64
	DiskTotalBytes   *uint64
	DiskUsedBytes    *uint64
	UptimeMS         *int64
	Load1m           *float64
}

// Read collects the figures; diskPath selects the filesystem to measure.
// CPU usage is sampled over sampleWindow (0 = a 500 ms default).
func Read(diskPath string, sampleWindow time.Duration) Stats {
	if sampleWindow <= 0 {
		sampleWindow = 500 * time.Millisecond
	}
	s := read(diskPath, sampleWindow)
	s.RecordedAt = time.Now()
	return s
}
