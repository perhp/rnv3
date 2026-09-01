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

// maxTLEAge before a refresh is forced at startup/replan time.
const maxTLEAge = 24 * time.Hour

// Run drives the scheduler until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.plan(ctx); err != nil {
		// Startup without network or TLEs is survivable: keep serving the web
		// panel and retry planning on the next cycle.
		slog.Error("initial planning failed; will retry", "err", err)
	}

	for {
		cfg := s.prov.Get()
		now := time.Now()
		next, err := s.st.NextScheduled(now)
		if err != nil {
			return err
		}

		wake := s.nextTLERefresh(cfg, now)
		wakeReason := "tle-refresh"
		if next != nil {
			aos := time.Unix(next.StartTS, 0)
			if aos.Before(now) {
				aos = now // pass already in progress: capture the remaining window
			}
			if aos.Before(wake) {
				wake = aos
				wakeReason = "pass"
			}
			slog.Info("scheduler waiting", "until", wake.Format(time.RFC3339), "reason", wakeReason,
				"next_pass", next.Satellite, "aos", aos.Format(time.RFC3339), "max_elev", int(next.MaxElevation))
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
			if err := s.plan(ctx); err != nil {
				slog.Error("replan failed", "err", err)
			}
		case <-timer.C:
			if wakeReason == "pass" && next != nil {
				// The runner claims the row scheduled→capturing atomically, so
				// a pass cancelled from the admin page while the timer was
				// pending is a no-op here.
				s.runner.Run(ctx, *next, satByName(cfg, next.Satellite))
			} else {
				if err := s.plan(ctx); err != nil {
					slog.Error("scheduled replan failed", "err", err)
				}
			}
		}
	}
}

// plan refreshes TLEs when stale, sweeps zombie passes, and rebuilds +
// persists the pass plan.
func (s *Scheduler) plan(ctx context.Context) error {
	cfg := s.prov.Get()
	var ids []int
	for _, sat := range cfg.EnabledSatellites() {
		ids = append(ids, sat.NoradID)
	}

	set, _, err := s.tles.Load()
	needFetch := err != nil || s.tles.Age() > maxTLEAge
	if !needFetch {
		for _, id := range ids {
			if _, ok := set[id]; !ok {
				needFetch = true
				break
			}
		}
	}
	if needFetch {
		fresh, ferr := s.tles.Refresh(ctx, ids)
		if ferr != nil {
			if set == nil {
				return ferr // no cache and no network: cannot plan at all
			}
			slog.Warn("TLE refresh failed, planning with cached set", "age", s.tles.Age().Round(time.Minute), "err", ferr)
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
		return err
	}
	if err := s.st.ReplaceFuturePlan(now, ToStorePasses(cands)); err != nil {
		return err
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
	return nil
}

// nextTLERefresh returns the next daily refresh instant (configured UTC hour,
// validated to 0..23 at config load; clamped anyway for defense in depth).
func (s *Scheduler) nextTLERefresh(cfg *config.Config, now time.Time) time.Time {
	hour := cfg.Scheduling.TLERefreshHourUTC
	if hour < 0 || hour > 23 {
		hour = 0
	}
	u := now.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day(), hour, 1, 0, 0, time.UTC)
	if !next.After(u) {
		next = next.Add(24 * time.Hour)
	}
	return next
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
