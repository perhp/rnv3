package sched

import (
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

func refreshCfg(hourUTC int) *config.Config {
	return &config.Config{Scheduling: config.Scheduling{TLERefreshHourUTC: hourUTC}}
}

func oct(day, hour, min, sec int) time.Time {
	return time.Date(2026, 10, day, hour, min, sec, 0, time.UTC)
}

func TestTLEStale(t *testing.T) {
	cases := []struct {
		name      string
		hour      int
		fetchedAt time.Time
		now       time.Time
		want      bool
	}{
		// The daily refresh fires at the same time each day and yesterday's
		// download finished a few seconds after it, so the set is just under
		// 24 h old. A 24 h age check skipped this refresh every other day.
		{"yesterday's refresh, seconds short of 24 h", 0, oct(9, 0, 37, 4), oct(10, 0, 37, 0), true},
		{"fetched after today's refresh time", 0, oct(10, 0, 37, 4), oct(10, 18, 0, 0), false},
		{"refresh time not reached yet today", 0, oct(9, 0, 37, 4), oct(10, 0, 30, 0), false},
		{"fetched hours before the refresh time", 0, oct(9, 21, 0, 0), oct(10, 0, 37, 0), true},
		{"fetched minutes before the refresh time", 0, oct(10, 0, 0, 0), oct(10, 0, 37, 0), false},
		{"several days old", 0, oct(7, 0, 37, 4), oct(10, 20, 0, 0), true},
		{"refresh hour 3, reached", 3, oct(10, 0, 37, 0), oct(10, 3, 37, 0), true},
		{"refresh hour 3, not reached", 3, oct(10, 0, 37, 0), oct(10, 3, 0, 0), false},
	}
	for _, c := range cases {
		if got := tleStale(refreshCfg(c.hour), c.fetchedAt, c.now); got != c.want {
			t.Errorf("%s: tleStale = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTLEScheduleDue(t *testing.T) {
	cases := []struct {
		name    string
		hour    int
		sched   tleSchedule
		now     time.Time
		wantDue time.Time
	}{
		{"today's refresh done", 0, tleSchedule{refreshed: oct(10, 0, 37, 0)}, oct(10, 5, 0, 0), oct(11, 0, 37, 0)},
		{"today's refresh still ahead", 0, tleSchedule{refreshed: oct(9, 0, 37, 0)}, oct(10, 0, 30, 0), oct(10, 0, 37, 0)},
		{"today's refresh missed during a capture", 0, tleSchedule{refreshed: oct(9, 0, 37, 0)}, oct(10, 0, 55, 0), oct(10, 0, 37, 0)},
		{"refresh hour moved by a reload", 3, tleSchedule{refreshed: oct(10, 0, 37, 0)}, oct(10, 2, 0, 0), oct(10, 3, 37, 0)},
		{"failed download retried", 0, tleSchedule{refreshed: oct(12, 0, 37, 0), retryAt: oct(12, 1, 10, 0)}, oct(12, 0, 40, 0), oct(12, 1, 10, 0)},
		{"retry never delays the daily refresh", 0, tleSchedule{refreshed: oct(11, 0, 37, 0), retryAt: oct(12, 0, 50, 0)}, oct(12, 0, 30, 0), oct(12, 0, 37, 0)},
	}
	for _, c := range cases {
		if got := c.sched.due(refreshCfg(c.hour), c.now); !got.Equal(c.wantDue) {
			t.Errorf("%s: due %s, want %s", c.name, got, c.wantDue)
		}
	}
}

// Celestrak timed out and returned 503s at the daily refresh, and the
// loop waited a whole day for the next one. It must retry every half hour
// until a download succeeds, then go back to the daily time.
func TestTLEScheduleRetriesFailedDownload(t *testing.T) {
	cfg := refreshCfg(0)
	refresh := tleSchedule{refreshed: oct(12, 0, 37, 0)}

	refresh.planned(true, oct(12, 0, 40, 10))
	if due := refresh.due(cfg, oct(12, 0, 40, 10)); !due.Equal(oct(12, 1, 10, 10)) {
		t.Fatalf("after a failed download: due %s, want a retry at 01:10:10", due)
	}
	refresh.planned(true, oct(12, 1, 13, 10))
	if due := refresh.due(cfg, oct(12, 1, 13, 10)); !due.Equal(oct(12, 1, 43, 10)) {
		t.Fatalf("after a second failure: due %s, want a retry at 01:43:10", due)
	}
	refresh.planned(false, oct(12, 1, 43, 20))
	if due := refresh.due(cfg, oct(12, 1, 43, 20)); !due.Equal(oct(13, 0, 37, 0)) {
		t.Fatalf("after a successful download: due %s, want tomorrow's refresh", due)
	}
}

func TestNextWake(t *testing.T) {
	now := oct(10, 0, 55, 0)
	pass := func(aos time.Time) *store.Pass { return &store.Pass{Satellite: "METEOR-M2 4", StartTS: aos.Unix()} }
	cases := []struct {
		name       string
		refreshDue time.Time
		next       *store.Pass
		wantWake   time.Time
		wantReason string
	}{
		{"no pass planned", oct(11, 0, 37, 0), nil, oct(11, 0, 37, 0), "tle-refresh"},
		{"pass before the refresh", oct(11, 0, 37, 0), pass(oct(10, 3, 0, 0)), oct(10, 3, 0, 0), "pass"},
		{"refresh before the pass", oct(11, 0, 37, 0), pass(oct(11, 2, 0, 0)), oct(11, 0, 37, 0), "tle-refresh"},
		// A capture that ran across the refresh time held the loop: the
		// refresh runs straight away instead of a day later.
		{"overdue refresh", oct(10, 0, 37, 0), pass(oct(10, 3, 0, 0)), oct(10, 0, 37, 0), "tle-refresh"},
		{"pass in progress beats an overdue refresh", oct(10, 0, 37, 0), pass(oct(10, 0, 50, 0)), now, "pass"},
	}
	for _, c := range cases {
		wake, reason := nextWake(now, c.refreshDue, c.next)
		if !wake.Equal(c.wantWake) || reason != c.wantReason {
			t.Errorf("%s: wake %s (%s), want %s (%s)", c.name, wake, reason, c.wantWake, c.wantReason)
		}
	}
}
