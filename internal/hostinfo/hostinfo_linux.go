//go:build linux

package hostinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func read(diskPath string, window time.Duration) Stats {
	var s Stats
	if raw, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if milli, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			t := float64(milli) / 1000
			s.CPUTemperatureC = &t
		}
	}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			if load, err := strconv.ParseFloat(f[0], 64); err == nil {
				s.Load1m = &load
			}
		}
	}
	if total, avail, ok := readMemInfo(); ok {
		used := total - avail
		s.MemoryTotalBytes, s.MemoryUsedBytes = &total, &used
	}
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil {
				ms := int64(up * 1000)
				s.UptimeMS = &ms
			}
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &st); err == nil && st.Blocks > 0 {
		total := uint64(st.Blocks) * uint64(st.Bsize)
		used := (uint64(st.Blocks) - uint64(st.Bfree)) * uint64(st.Bsize)
		s.DiskTotalBytes, s.DiskUsedBytes = &total, &used
	}
	if a, ok := readCPUTimes(); ok {
		time.Sleep(window)
		if b, ok := readCPUTimes(); ok {
			if pct, ok := cpuUsage(a, b); ok {
				s.CPUUsagePercent = &pct
			}
		}
	}
	return s
}

// readMemInfo returns MemTotal and MemAvailable in bytes.
func readMemInfo() (total, avail uint64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	return total, avail, total > 0
}

func readCPUTimes() (cpuTimes, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	return parseCPUTimes(string(raw))
}
