package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// realertInterval: each distinct alert is re-sent at most this often so a
// persistent failure doesn't flood the channels (RN2: 24 h).
const realertInterval = 24 * time.Hour

// minAttemptsForFailureAlert: "all passes failing" needs a few samples.
const minAttemptsForFailureAlert = 3

// watchdog is RN2's health_watchdog.sh: checks that the station is actually
// producing captures and alerts through the push channels when not.
type watchdog struct {
	j         *Jobs
	mu        sync.Mutex
	lastAlert map[string]time.Time
	statePath string
}

func newWatchdog(j *Jobs) *watchdog {
	w := &watchdog{j: j, lastAlert: map[string]time.Time{}}
	if j.StateDir != "" {
		w.statePath = filepath.Join(j.StateDir, "watchdog-state.json")
		w.load()
	}
	return w
}

func (w *watchdog) load() {
	raw, err := os.ReadFile(w.statePath)
	if err != nil {
		return
	}
	var m map[string]int64
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for k, ts := range m {
		w.lastAlert[k] = time.Unix(ts, 0)
	}
}

func (w *watchdog) save() {
	if w.statePath == "" {
		return
	}
	m := map[string]int64{}
	for k, t := range w.lastAlert {
		m[k] = t.Unix()
	}
	raw, _ := json.Marshal(m)
	os.WriteFile(w.statePath, raw, 0o644)
}

// alert sends unless the same check alerted within realertInterval.
func (w *watchdog) alert(ctx context.Context, now time.Time, check, message string) {
	w.mu.Lock()
	last, ok := w.lastAlert[check]
	if ok && now.Sub(last) < realertInterval {
		w.mu.Unlock()
		logger().Warn("watchdog: still failing (re-alert suppressed)", "check", check, "message", message)
		return
	}
	w.lastAlert[check] = now
	w.save()
	w.mu.Unlock()
	if w.j.Notify != nil {
		w.j.Notify.Alert(ctx, check, message)
	}
}

// clear forgets a check's alert once it is healthy again, so the next
// failure alerts immediately.
func (w *watchdog) clear(check string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.lastAlert[check]; ok {
		delete(w.lastAlert, check)
		w.save()
	}
}

func (w *watchdog) runOnce(ctx context.Context, now time.Time) {
	cfg := w.j.Prov.Get()
	if !cfg.Watchdog.Enabled {
		return
	}
	st := w.j.St
	log := logger()

	maxHours := cfg.Watchdog.MaxHoursWithoutCapture
	if maxHours <= 0 {
		maxHours = 48
	}
	if n, err := st.DecodedCountSince(now.Add(-time.Duration(maxHours) * time.Hour)); err == nil {
		if n == 0 {
			w.alert(ctx, now, "no_recent_capture",
				fmt.Sprintf("no successfully decoded capture in the last %d hours - check the rnv3 log (journalctl -u rnv3)", maxHours))
		} else {
			w.clear("no_recent_capture")
		}
	}

	if attempted, failed, err := st.AttemptedSince(now.Add(-24*time.Hour), now); err == nil {
		if attempted >= minAttemptsForFailureAlert && failed == attempted {
			w.alert(ctx, now, "all_passes_failing",
				fmt.Sprintf("all %d passes in the last 24 hours failed - check the SDR and the rnv3 log", attempted))
		} else {
			w.clear("all_passes_failing")
		}
	}

	threshold := cfg.Watchdog.DiskUsageThresholdPct
	if threshold <= 0 {
		threshold = 90
	}
	if pct, ok := diskUsagePercent(cfg.Paths.Images); ok {
		if pct >= threshold {
			w.alert(ctx, now, "disk_usage",
				fmt.Sprintf("image storage is %d%% full (threshold %d%%) - prune captures or lower retention settings", pct, threshold))
		} else {
			w.clear("disk_usage")
		}
	}

	if next, err := st.NextPass(now); err == nil {
		if next == nil {
			w.alert(ctx, now, "no_scheduled_passes",
				"no passes are scheduled - pass planning appears broken (TLE download failure, or every satellite disabled?)")
		} else {
			w.clear("no_scheduled_passes")
		}
	}

	if cfg.SDR.Type == "rtlsdr" {
		if present, probed := rtlsdrPresent(); probed {
			if !present {
				w.alert(ctx, now, "sdr_missing", "no RTL-SDR device visible on USB - is the dongle unplugged or hung?")
			} else {
				w.clear("sdr_missing")
			}
		}
	}
	log.Debug("watchdog run complete")
}
