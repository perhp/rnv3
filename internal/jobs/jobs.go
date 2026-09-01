// Package jobs runs the station's timed housekeeping inside the daemon —
// what RN2 wired up as cron entries: the hourly health watchdog, the
// evening best-of-day push, and retention pruning.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

// Notifier is the subset of notify.Notifier the jobs use.
type Notifier interface {
	Alert(ctx context.Context, check, message string)
	DailySummary(ctx context.Context, annotation string, files []string)
}

// Jobs bundles the dependencies shared by the housekeeping tasks.
type Jobs struct {
	Prov   *config.Provider
	St     *store.Store
	Notify Notifier
	// StateDir persists watchdog alert timestamps across restarts.
	StateDir string
}

const (
	watchdogInterval = time.Hour
	watchdogFirstRun = 2 * time.Minute
	pruneFirstRun    = 5 * time.Minute
	pruneHour        = 3 // local 03:15, when nothing else runs
	pruneMinute      = 15
)

// Run starts the three loops and blocks until ctx is cancelled.
func (j *Jobs) Run(ctx context.Context) {
	wd := newWatchdog(j)
	go runEvery(ctx, watchdogFirstRun, watchdogInterval, func(now time.Time) { wd.runOnce(ctx, now) })
	go runDailyAt(ctx, func() (int, int) {
		h, m, _ := config.ParseClock(j.Prov.Get().Daily.PushTime)
		return h, m
	}, func(now time.Time) { j.bestOfDay(ctx, now) })
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pruneFirstRun):
			j.prune(time.Now())
		}
		runDailyAt(ctx, func() (int, int) { return pruneHour, pruneMinute }, func(now time.Time) { j.prune(now) })
	}()
	<-ctx.Done()
}

// runEvery calls fn after first, then every interval, until ctx ends.
func runEvery(ctx context.Context, first, interval time.Duration, fn func(time.Time)) {
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			fn(now)
			timer.Reset(interval)
		}
	}
}

// runDailyAt calls fn at the next local occurrence of the clock time
// returned by at (re-read each day so a config reload applies).
func runDailyAt(ctx context.Context, at func() (int, int), fn func(time.Time)) {
	for {
		h, m := at()
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case fired := <-timer.C:
			fn(fired)
		}
	}
}

func logger() *slog.Logger { return slog.Default().With("component", "jobs") }
