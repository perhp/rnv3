package store

import (
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var now = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func plannedPass(sat string, startOffsetMin int, elev float64, state string) Pass {
	start := now.Add(time.Duration(startOffsetMin) * time.Minute)
	return Pass{
		Satellite:    sat,
		StartTS:      start.Unix(),
		EndTS:        start.Add(15 * time.Minute).Unix(),
		MaxElevation: elev,
		Direction:    "southbound",
		State:        state,
	}
}

func TestReplaceFuturePlanInsertsAndLists(t *testing.T) {
	s := testStore(t)
	plan := []Pass{
		plannedPass("NOAA 19", 60, 45, StateScheduled),
		plannedPass("METEOR-M2 3", 120, 60, StateScheduled),
		plannedPass("NOAA 18", 90, 30, StateSkipped),
	}
	if err := s.ReplaceFuturePlan(now, plan); err != nil {
		t.Fatal(err)
	}
	got, err := s.UpcomingPasses(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d upcoming passes, want 3", len(got))
	}
	if got[0].Satellite != "NOAA 19" || got[1].Satellite != "NOAA 18" || got[2].Satellite != "METEOR-M2 3" {
		t.Errorf("wrong order: %s, %s, %s", got[0].Satellite, got[1].Satellite, got[2].Satellite)
	}

	next, err := s.NextScheduled(now)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.Satellite != "NOAA 19" {
		t.Errorf("NextScheduled = %+v, want NOAA 19", next)
	}
}

func TestReplaceFuturePlanIsIdempotent(t *testing.T) {
	s := testStore(t)
	plan := []Pass{plannedPass("NOAA 19", 60, 45, StateScheduled)}
	for i := 0; i < 3; i++ {
		if err := s.ReplaceFuturePlan(now, plan); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.UpcomingPasses(now, 10)
	if len(got) != 1 {
		t.Fatalf("replanning duplicated rows: %d", len(got))
	}
}

func TestReplaceFuturePlanPreservesCancellations(t *testing.T) {
	s := testStore(t)
	plan := []Pass{
		plannedPass("NOAA 19", 60, 45, StateScheduled),
		plannedPass("METEOR-M2 3", 120, 60, StateScheduled),
	}
	if err := s.ReplaceFuturePlan(now, plan); err != nil {
		t.Fatal(err)
	}
	next, _ := s.NextScheduled(now)
	if err := s.SetPassState(next.ID, StateCancelled, "cancelled by user"); err != nil {
		t.Fatal(err)
	}

	// Replan with the same prediction, shifted by a few seconds (SGP4 drift).
	shifted := make([]Pass, len(plan))
	copy(shifted, plan)
	shifted[0].StartTS += 30
	if err := s.ReplaceFuturePlan(now, shifted); err != nil {
		t.Fatal(err)
	}

	got, _ := s.UpcomingPasses(now, 10)
	if len(got) != 2 {
		t.Fatalf("got %d passes after replan, want 2 (cancelled + meteor)", len(got))
	}
	if got[0].State != StateCancelled {
		t.Errorf("cancellation lost on replan: state=%s", got[0].State)
	}
	nextAfter, _ := s.NextScheduled(now)
	if nextAfter == nil || nextAfter.Satellite != "METEOR-M2 3" {
		t.Errorf("NextScheduled should skip the cancelled pass, got %+v", nextAfter)
	}
}

func TestReplaceFuturePlanKeepsTerminalStates(t *testing.T) {
	s := testStore(t)
	past := []Pass{plannedPass("NOAA 19", 60, 45, StateScheduled)}
	if err := s.ReplaceFuturePlan(now, past); err != nil {
		t.Fatal(err)
	}
	next, _ := s.NextScheduled(now)
	if err := s.SetPassState(next.ID, StateDecoded, ""); err != nil {
		t.Fatal(err)
	}
	// Replan with an empty plan: the decoded pass must survive.
	if err := s.ReplaceFuturePlan(now, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UpcomingPasses(now, 10)
	if len(got) != 1 || got[0].State != StateDecoded {
		t.Fatalf("decoded pass lost on replan: %+v", got)
	}
}

func TestSetPassState(t *testing.T) {
	s := testStore(t)
	if err := s.ReplaceFuturePlan(now, []Pass{plannedPass("NOAA 19", 60, 45, StateScheduled)}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.NextScheduled(now)
	if err := s.SetPassState(p.ID, StateFailed, "no images decoded"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UpcomingPasses(now, 10)
	if got[0].State != StateFailed || got[0].ErrorText != "no images decoded" {
		t.Errorf("state transition not persisted: %+v", got[0])
	}
}
