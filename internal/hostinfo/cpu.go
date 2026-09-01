package hostinfo

import (
	"strconv"
	"strings"
)

// cpuTimes is the aggregate "cpu" line of /proc/stat.
type cpuTimes struct{ idle, total uint64 }

// parseCPUTimes reads the aggregate cpu line of /proc/stat:
// "cpu user nice system idle iowait irq softirq steal ...".
func parseCPUTimes(procStat string) (cpuTimes, bool) {
	for _, line := range strings.Split(procStat, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i, v := range f[1:] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return cpuTimes{}, false
			}
			t.total += n
			if i == 3 || i == 4 { // idle, iowait
				t.idle += n
			}
		}
		return t, true
	}
	return cpuTimes{}, false
}

// cpuUsage is the busy percentage between two samples.
func cpuUsage(a, b cpuTimes) (float64, bool) {
	total := b.total - a.total
	if b.total < a.total || total == 0 {
		return 0, false
	}
	idle := b.idle - a.idle
	pct := 100 * float64(total-idle) / float64(total)
	return float64(int(pct*10+0.5)) / 10, true
}
