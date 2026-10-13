package sched

import (
	"context"
	"log/slog"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

// CaptureRunner executes one pass. M2 provides the real SatDump runner;
// NotImplementedRunner stands in for dry-run mode.
type CaptureRunner interface {
	// Run owns the pass from AOS: it must move the pass out of the scheduled
	// state and leave it in a terminal state before returning.
	Run(ctx context.Context, p store.Pass, sat config.Satellite)
}

// Scheduler plans passes and fires the capture runner at AOS. It replaces
// RN2's cron + at + atq machinery with a single loop. Config is snapshotted
// from the provider once per iteration, so a SIGHUP reload takes effect at
// the next loop turn without racing an in-flight capture.
type Scheduler struct {
	prov   *config.Provider
	st     *store.Store
	tles   *tle.Manager
	runner CaptureRunner

	// OnPlanUpdated, when set, runs after every successful replan (the
	// event webhooks publish schedule.updated from it).
	OnPlanUpdated func(ctx context.Context)

	replanCh chan struct{}
}

func New(prov *config.Provider, st *store.Store, tles *tle.Manager, runner CaptureRunner) *Scheduler {
	return &Scheduler{prov: prov, st: st, tles: tles, runner: runner, replanCh: make(chan struct{}, 1)}
}

// Replan requests an asynchronous replan (config reload, manual trigger).
func (s *Scheduler) Replan() {
	select {
	case s.replanCh <- struct{}{}:
	default:
	}
}

// tleRefetchGap is the least time between two downloads of the TLE set:
// Celestrak updates its element sets about every two hours and turns away
// repeat downloads sooner than that.
const tleRefetchGap = 2 * time.Hour

// tleRetryInterval spaces out attempts while TLE downloads keep failing, so
// an outage costs half an hour instead of the rest of the day.
const tleRetryInterval = 30 * time.Minute

// tleRefreshMinute is the minute past tle_refresh_hour_utc the daily
// refresh runs at. Scheduled jobs cluster on the hour, and Celestrak timed
// out and returned 503s at 00:01 UTC, so the refresh keeps well clear of it.
const tleRefreshMinute = 37

// Run drives the scheduler until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	// The startup plan stands in for the daily refresh time just before
	// startup, taken before planning so a startup that straddles the
	// refresh time still leaves that refresh due.
	refresh := tleSchedule{refreshed: lastTLERefresh(s.prov.Get(), time.Now())}
	failed, err := s.plan(ctx)
	if err != nil {
		// Startup without network or TLEs is survivable: keep serving the web
		// panel and retry planning along with the TLE download.
		slog.Error("initial planning failed; will retry", "err", err)
	}
	refresh.planned(failed, time.Now())

	for {
		cfg := s.prov.Get()
		now := time.Now()
		next, err := s.st.NextScheduled(now)
		if err != nil {
			return err
		}

		wake, wakeReason := nextWake(now, refresh.due(cfg, now), next)
		if next != nil {
			slog.Info("scheduler waiting", "until", wake.Format(time.RFC3339), "reason", wakeReason,
				"next_pass", next.Satellite, "aos", time.Unix(next.StartTS, 0).Format(time.RFC3339), "max_elev", int(next.MaxElevation))
		} else {
			slog.Info("scheduler waiting (no passes planned)", "until", wake.Format(time.RFC3339), "reason", wakeReason)
		}

		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-s.replanCh:
			timer.Stop()
			failed, err := s.plan(ctx)
			if err != nil {
				slog.Error("replan failed", "err", err)
			}
			refresh.planned(failed, time.Now())
		case <-timer.C:
			if wakeReason == "pass" && next != nil {
				// The runner claims the row scheduled→capturing atomically, so
				// a pass cancelled from the admin page while the timer was
				// pending is a no-op here.
				s.runner.Run(ctx, *next, satByName(cfg, next.Satellite))
			} else {
				refresh.refreshed = lastTLERefresh(cfg, time.Now())
				failed, err := s.plan(ctx)
				if err != nil {
					slog.Error("scheduled replan failed", "err", err)
				}
				refresh.planned(failed, time.Now())
			}
		}
	}
}

// plan refreshes TLEs when stale, sweeps zombie passes, and rebuilds +
// persists the pass plan. The bool reports a TLE download that was needed
// but failed, for the loop to retry.
func (s *Scheduler) plan(ctx context.Context) (bool, error) {
	cfg := s.prov.Get()
	var ids []int
	for _, sat := range cfg.EnabledSatellites() {
		ids = append(ids, sat.NoradID)
	}

	set, fetchedAt, err := s.tles.Load()
	needFetch := err != nil || tleStale(cfg, fetchedAt, time.Now())
	if !needFetch {
		for _, id := range ids {
			if _, ok := set[id]; !ok {
				needFetch = true
				break
			}
		}
	}
	tleFailed := false
	if needFetch {
		fresh, ferr := s.tles.Refresh(ctx, ids)
		if ferr != nil {
			if set == nil {
				return true, ferr // no cache and no network: cannot plan at all
			}
			slog.Warn("TLE refresh failed, planning with cached set", "age", s.tles.Age().Round(time.Minute), "err", ferr)
			tleFailed = true
		} else {
			set = fresh
		}
	}

	now := time.Now()
	if n, err := s.st.MarkMissedScheduled(now); err != nil {
		slog.Warn("cannot sweep missed passes", "err", err)
	} else if n > 0 {
		slog.Info("marked missed passes", "count", n)
	}
	if n, err := s.st.FailStaleRunning(now); err != nil {
		slog.Warn("cannot sweep stale running passes", "err", err)
	} else if n > 0 {
		slog.Warn("failed passes stuck in capturing/processing", "count", n)
	}

	cands, err := BuildPlan(cfg, set, now)
	if err != nil {
		return tleFailed, err
	}
	if err := s.st.ReplaceFuturePlan(now, ToStorePasses(cands)); err != nil {
		return tleFailed, err
	}
	scheduled, skipped := 0, 0
	for _, c := range cands {
		if c.Skipped {
			skipped++
		} else {
			scheduled++
		}
	}
	slog.Info("pass plan updated", "scheduled", scheduled, "skipped", skipped,
		"window_days", cfg.Scheduling.DaysAhead)
	if s.OnPlanUpdated != nil {
		s.OnPlanUpdated(ctx)
	}
	return tleFailed, nil
}

// nextWake picks what the loop sleeps until: the next pass's AOS or the TLE
// refresh, whichever comes first. A pass already under way goes ahead of an
// overdue refresh, so a slow download never eats into the capture.
func nextWake(now, refreshDue time.Time, next *store.Pass) (time.Time, string) {
	if next != nil {
		aos := time.Unix(next.StartTS, 0)
		if !aos.After(now) {
			return now, "pass" // pass already in progress: capture the remaining window
		}
		if aos.Before(refreshDue) {
			return aos, "pass"
		}
	}
	return refreshDue, "tle-refresh"
}

// tleSchedule tracks when the loop next downloads TLEs.
type tleSchedule struct {
	refreshed time.Time // most recent daily refresh time the loop has dealt with
	retryAt   time.Time // while a download is failing: when to try again
}

// due returns when the next TLE download is due: a pending retry, or the
// most recent daily refresh time if the loop has not dealt with it yet (a
// capture was running when it came round), or else the one after it.
func (t tleSchedule) due(cfg *config.Config, now time.Time) time.Time {
	last := lastTLERefresh(cfg, now)
	due := last.Add(24 * time.Hour)
	if last.After(t.refreshed) {
		due = last
	}
	if !t.retryAt.IsZero() && t.retryAt.Before(due) {
		due = t.retryAt
	}
	return due
}

// planned records whether a plan's TLE download failed: a failure is
// retried after tleRetryInterval, a success (or no download needed) clears
// any pending retry.
func (t *tleSchedule) planned(failed bool, now time.Time) {
	t.retryAt = time.Time{}
	if failed {
		t.retryAt = now.Add(tleRetryInterval)
	}
}

// tleStale reports whether a TLE set fetched at fetchedAt needs downloading
// again: it predates the most recent daily refresh time. Measuring against
// that instant, not a 24 h age, matters because the refresh fires at the
// same time each day and finds the previous download a few seconds short
// of 24 h old.
func tleStale(cfg *config.Config, fetchedAt, now time.Time) bool {
	return fetchedAt.Before(lastTLERefresh(cfg, now)) && now.Sub(fetchedAt) >= tleRefetchGap
}

// lastTLERefresh returns the most recent daily refresh instant at or before
// now (configured UTC hour, validated to 0..23 at config load; clamped
// anyway for defense in depth).
func lastTLERefresh(cfg *config.Config, now time.Time) time.Time {
	hour := cfg.Scheduling.TLERefreshHourUTC
	if hour < 0 || hour > 23 {
		hour = 0
	}
	u := now.UTC()
	last := time.Date(u.Year(), u.Month(), u.Day(), hour, tleRefreshMinute, 0, 0, time.UTC)
	if last.After(u) {
		last = last.Add(-24 * time.Hour)
	}
	return last
}

func satByName(cfg *config.Config, name string) config.Satellite {
	for _, sat := range cfg.Satellites {
		if sat.Name == name {
			return sat
		}
	}
	return config.Satellite{Name: name}
}

// NotImplementedRunner is the dry-run capture runner: it marks the pass
// skipped with an honest reason instead of touching the SDR.
type NotImplementedRunner struct {
	St     *store.Store
	DryRun bool
}

func (r *NotImplementedRunner) Run(ctx context.Context, p store.Pass, sat config.Satellite) {
	reason := "capture pipeline disabled"
	if r.DryRun {
		reason = "dry-run mode"
	}
	slog.Info("pass AOS reached — would capture", "satellite", p.Satellite,
		"max_elev", int(p.MaxElevation), "direction", p.Direction, "reason", reason)
	if err := r.St.SetPassState(p.ID, store.StateSkipped, reason); err != nil {
		slog.Error("failed to mark pass skipped", "id", p.ID, "err", err)
	}
}
