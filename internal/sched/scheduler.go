package sched

import (
	"context"
	"log/slog"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

// CaptureRunner executes one pass. M2 provides the real SatDump runner; until
// then NotImplementedRunner records why nothing was captured.
type CaptureRunner interface {
	// Run owns the pass from AOS: it must move the pass out of the scheduled
	// state and leave it in a terminal state before returning.
	Run(ctx context.Context, p store.Pass, sat config.Satellite)
}

// Scheduler plans passes and fires the capture runner at AOS. It replaces
// RN2's cron + at + atq machinery with a single loop.
type Scheduler struct {
	cfg    *config.Config
	st     *store.Store
	tles   *tle.Manager
	runner CaptureRunner

	replanCh chan struct{}
}

func New(cfg *config.Config, st *store.Store, tles *tle.Manager, runner CaptureRunner) *Scheduler {
	return &Scheduler{cfg: cfg, st: st, tles: tles, runner: runner, replanCh: make(chan struct{}, 1)}
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
		now := time.Now()
		next, err := s.st.NextScheduled(now)
		if err != nil {
			return err
		}

		wake := s.nextTLERefresh(now)
		wakeReason := "tle-refresh"
		if next != nil {
			aos := time.Unix(next.StartTS, 0)
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
				s.runner.Run(ctx, *next, s.satByName(next.Satellite))
			} else {
				if err := s.plan(ctx); err != nil {
					slog.Error("scheduled replan failed", "err", err)
				}
			}
		}
	}
}

// plan refreshes TLEs when stale and rebuilds + persists the pass plan.
func (s *Scheduler) plan(ctx context.Context) error {
	var ids []int
	for _, sat := range s.cfg.EnabledSatellites() {
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
	cands, err := BuildPlan(s.cfg, set, now)
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
		"window_days", s.cfg.Scheduling.DaysAhead)
	return nil
}

// nextTLERefresh returns the next daily refresh instant (configured UTC hour).
func (s *Scheduler) nextTLERefresh(now time.Time) time.Time {
	u := now.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day(), s.cfg.Scheduling.TLERefreshHourUTC, 1, 0, 0, time.UTC)
	if !next.After(u) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func (s *Scheduler) satByName(name string) config.Satellite {
	for _, sat := range s.cfg.Satellites {
		if sat.Name == name {
			return sat
		}
	}
	return config.Satellite{Name: name}
}

// NotImplementedRunner is the M1 placeholder capture runner: it marks the
// pass skipped, with a reason that distinguishes dry-run mode from the
// capture pipeline simply not existing yet.
type NotImplementedRunner struct {
	St     *store.Store
	DryRun bool
}

func (r *NotImplementedRunner) Run(ctx context.Context, p store.Pass, sat config.Satellite) {
	reason := "capture pipeline not implemented yet (M2)"
	if r.DryRun {
		reason = "dry-run mode"
	}
	slog.Info("pass AOS reached — would capture", "satellite", p.Satellite,
		"max_elev", int(p.MaxElevation), "direction", p.Direction, "reason", reason)
	if err := r.St.SetPassState(p.ID, store.StateSkipped, reason); err != nil {
		slog.Error("failed to mark pass skipped", "id", p.ID, "err", err)
	}
}
