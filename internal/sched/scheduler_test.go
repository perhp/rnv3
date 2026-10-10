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
		// The daily refresh fires at 00:01 and yesterday's download finished
		// a few seconds after it, so the set is just under 24 h old. A 24 h
		// age check skipped this refresh every other day.
		{"yesterday's refresh, seconds short of 24 h", 0, oct(9, 0, 1, 4), oct(10, 0, 1, 0), true},
		{"fetched after today's refresh time", 0, oct(10, 0, 1, 4), oct(10, 18, 0, 0), false},
		{"refresh time not reached yet today", 0, oct(9, 0, 1, 4), oct(9, 23, 59, 0), false},
		{"fetched hours before the refresh time", 0, oct(9, 21, 0, 0), oct(10, 0, 1, 0), true},
		{"fetched minutes before the refresh time", 0, oct(9, 23, 30, 0), oct(10, 0, 1, 0), false},
		{"several days old", 0, oct(7, 0, 1, 4), oct(10, 20, 0, 0), true},
		{"refresh hour 12, reached", 12, oct(10, 6, 0, 0), oct(10, 12, 1, 0), true},
		{"refresh hour 12, not reached", 12, oct(10, 6, 0, 0), oct(10, 11, 0, 0), false},
	}
	for _, c := range cases {
		if got := tleStale(refreshCfg(c.hour), c.fetchedAt, c.now); got != c.want {
			t.Errorf("%s: tleStale = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTLERefreshDue(t *testing.T) {
	cases := []struct {
		name      string
		hour      int
		refreshed time.Time
		now       time.Time
		want      time.Time
	}{
		{"today's refresh done", 0, oct(10, 0, 1, 0), oct(10, 5, 0, 0), oct(11, 0, 1, 0)},
		{"today's refresh still ahead", 0, oct(9, 0, 1, 0), oct(10, 0, 0, 30), oct(10, 0, 1, 0)},
		{"today's refresh missed during a capture", 0, oct(9, 0, 1, 0), oct(10, 0, 25, 0), oct(10, 0, 1, 0)},
		{"refresh hour moved by a reload", 12, oct(10, 0, 1, 0), oct(10, 6, 0, 0), oct(10, 12, 1, 0)},
	}
	for _, c := range cases {
		if got := tleRefreshDue(refreshCfg(c.hour), c.refreshed, c.now); !got.Equal(c.want) {
			t.Errorf("%s: due %s, want %s", c.name, got, c.want)
		}
	}
}

func TestNextWake(t *testing.T) {
	now := oct(10, 0, 25, 0)
	pass := func(aos time.Time) *store.Pass { return &store.Pass{Satellite: "METEOR-M2 4", StartTS: aos.Unix()} }
	cases := []struct {
		name       string
		refreshDue time.Time
		next       *store.Pass
		wantWake   time.Time
		wantReason string
	}{
		{"no pass planned", oct(11, 0, 1, 0), nil, oct(11, 0, 1, 0), "tle-refresh"},
		{"pass before the refresh", oct(11, 0, 1, 0), pass(oct(10, 3, 0, 0)), oct(10, 3, 0, 0), "pass"},
		{"refresh before the pass", oct(11, 0, 1, 0), pass(oct(11, 2, 0, 0)), oct(11, 0, 1, 0), "tle-refresh"},
		// A capture that ran across the refresh time held the loop: the
		// refresh runs straight away instead of a day later.
		{"overdue refresh", oct(10, 0, 1, 0), pass(oct(10, 3, 0, 0)), oct(10, 0, 1, 0), "tle-refresh"},
		{"pass in progress beats an overdue refresh", oct(10, 0, 1, 0), pass(oct(10, 0, 20, 0)), now, "pass"},
	}
	for _, c := range cases {
		wake, reason := nextWake(now, c.refreshDue, c.next)
		if !wake.Equal(c.wantWake) || reason != c.wantReason {
			t.Errorf("%s: wake %s (%s), want %s (%s)", c.name, wake, reason, c.wantWake, c.wantReason)
		}
	}
}
