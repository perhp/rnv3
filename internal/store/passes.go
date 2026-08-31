package store

import (
	"database/sql"
	"errors"
	"time"
)

// Pass states.
const (
	StateScheduled  = "scheduled"
	StateCapturing  = "capturing"
	StateProcessing = "processing"
	StateDecoded    = "decoded"
	StateFailed     = "failed"
	StateSkipped    = "skipped"
	StateCancelled  = "cancelled"
)

// Pass mirrors a row of the passes table (planning-relevant columns; decode
// metrics columns are written by the capture pipeline in M2+).
type Pass struct {
	ID           int64
	Satellite    string
	StartTS      int64
	EndTS        int64
	MaxElevation float64
	StartAzimuth float64
	AzimuthAtMax float64
	Direction    string
	State        string
	ErrorText    string
}

// cancelMatchTolerance: a replan may shift a predicted pass by a few seconds;
// a user-cancelled pass within this window of a new prediction stays cancelled.
const cancelMatchTolerance = 120

// ReplaceFuturePlan atomically swaps the future portion of the plan: rows in
// planning states (scheduled/skipped) with start_ts > now are deleted and the
// new plan inserted. Rows in any other state — running, completed, failed, or
// user-cancelled — are preserved, and a new prediction matching a cancelled
// row is dropped so cancellations survive replans.
func (s *Store) ReplaceFuturePlan(now time.Time, planned []Pass) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT satellite, start_ts FROM passes WHERE start_ts > ? AND state = ?`,
		now.Unix(), StateCancelled)
	if err != nil {
		return err
	}
	type key struct {
		sat string
		ts  int64
	}
	var cancelled []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.sat, &k.ts); err != nil {
			rows.Close()
			return err
		}
		cancelled = append(cancelled, k)
	}
	rows.Close()

	if _, err := tx.Exec(
		`DELETE FROM passes WHERE start_ts > ? AND state IN (?, ?)`,
		now.Unix(), StateScheduled, StateSkipped); err != nil {
		return err
	}

	ins, err := tx.Prepare(`INSERT INTO passes
		(satellite, start_ts, end_ts, max_elevation, start_azimuth, azimuth_at_max, direction, state, error_text, updated_ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s','now'))
		ON CONFLICT (satellite, start_ts) DO NOTHING`)
	if err != nil {
		return err
	}
	defer ins.Close()

nextPass:
	for _, p := range planned {
		for _, c := range cancelled {
			if c.sat == p.Satellite && p.StartTS >= c.ts-cancelMatchTolerance && p.StartTS <= c.ts+cancelMatchTolerance {
				continue nextPass
			}
		}
		if _, err := ins.Exec(p.Satellite, p.StartTS, p.EndTS, p.MaxElevation,
			p.StartAzimuth, p.AzimuthAtMax, p.Direction, p.State, nullIfEmpty(p.ErrorText)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpcomingPasses lists passes that have not ended yet, soonest first.
func (s *Store) UpcomingPasses(now time.Time, limit int) ([]Pass, error) {
	rows, err := s.DB.Query(`SELECT id, satellite, start_ts, end_ts, max_elevation,
			COALESCE(start_azimuth, 0), COALESCE(azimuth_at_max, 0),
			COALESCE(direction, ''), state, COALESCE(error_text, '')
		FROM passes WHERE end_ts > ? ORDER BY start_ts LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pass
	for rows.Next() {
		var p Pass
		if err := rows.Scan(&p.ID, &p.Satellite, &p.StartTS, &p.EndTS, &p.MaxElevation,
			&p.StartAzimuth, &p.AzimuthAtMax, &p.Direction, &p.State, &p.ErrorText); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NextScheduled returns the next pass in scheduled state whose window has not
// closed yet — including one already in progress (daemon started mid-pass, or
// the previous capture overran this AOS), so the remaining window can still
// be captured. Nil when the plan is empty.
func (s *Store) NextScheduled(now time.Time) (*Pass, error) {
	p := &Pass{}
	err := s.DB.QueryRow(`SELECT id, satellite, start_ts, end_ts, max_elevation,
			COALESCE(start_azimuth, 0), COALESCE(azimuth_at_max, 0),
			COALESCE(direction, ''), state, COALESCE(error_text, '')
		FROM passes WHERE state = ? AND end_ts > ? ORDER BY start_ts LIMIT 1`,
		StateScheduled, now.Unix()).
		Scan(&p.ID, &p.Satellite, &p.StartTS, &p.EndTS, &p.MaxElevation,
			&p.StartAzimuth, &p.AzimuthAtMax, &p.Direction, &p.State, &p.ErrorText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// SetPassState transitions a pass and stamps updated_ts.
func (s *Store) SetPassState(id int64, state, errorText string) error {
	_, err := s.DB.Exec(
		`UPDATE passes SET state = ?, error_text = ?, updated_ts = strftime('%s','now') WHERE id = ?`,
		state, nullIfEmpty(errorText), id)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
