//go:build linux

package web

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hostVitals reads the idle-dashboard readings from /sys and /proc. Every
// reading is optional: a missing entry drops that key from the map.
func hostVitals(imagesDir string) map[string]any {
	v := map[string]any{}
	if raw, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if milli, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			v["cpu_temp"] = float64(milli/100) / 10
		}
	}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			if load, err := strconv.ParseFloat(f[0], 64); err == nil {
				v["load"] = load
			}
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		var total, avail int64
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				total = n
			case "MemAvailable:":
				avail = n
			}
		}
		f.Close()
		if total > 0 {
			v["mem_used_pct"] = int(100 - (100*avail)/total)
		}
	}
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil {
				v["uptime"] = int64(up)
			}
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(imagesDir, &st); err == nil {
		v["disk_free"] = uint64(st.Bavail) * uint64(st.Bsize)
	}
	return v
}

// tzName returns the station's IANA time zone name when it can be read from
// the system, else the abbreviation.
func tzName(now time.Time) string {
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil && strings.TrimSpace(string(raw)) != "" {
		return strings.TrimSpace(string(raw))
	}
	name, _ := now.Zone()
	return name
}
