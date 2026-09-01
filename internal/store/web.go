package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchedulePass is a pass row as the panel and the JSON API see it: the
// planning columns plus the decode metrics, with nullable fields as pointers
// so "unknown" renders as an em dash instead of 0 (RN2 parity).
type SchedulePass struct {
	ID              int64
	Satellite       string
	StartTS         int64
	EndTS           int64
	MaxElevation    float64
	StartAzimuth    *float64
	AzimuthAtMax    *float64
	Direction       string
	State           string
	ErrorText       string
	FileBase        string
	Daylight        bool
	Gain            *float64
	MaxSNR          *float64
	AvgSNR          *float64
	FramesReceived  *int
	FramesExpected  *int
	FrameLossPct    *float64
	LargestFrameGap *int
	// ThumbPath is the registered website thumbnail (thumbs-relative), or
	// "" when the capture has none (imported NOAA history).
	ThumbPath string
}

// Decoded reports whether the pass produced a capture.
func (p SchedulePass) Decoded() bool { return p.State == StateDecoded }

const schedulePassCols = `p.id, p.satellite, p.start_ts, p.end_ts, p.max_elevation,
	p.start_azimuth, p.azimuth_at_max, COALESCE(p.direction, ''), p.state, COALESCE(p.error_text, ''),
	COALESCE(p.file_base, ''), COALESCE(p.daylight, 0), p.gain, p.max_snr, p.avg_snr,
	p.frames_received, p.frames_expected, p.frame_loss_pct, p.largest_frame_gap,
	COALESCE(t.thumb_path, '')`

const schedulePassFrom = ` FROM passes p
	LEFT JOIN images t ON t.pass_id = p.id AND t.kind = 'website-thumbnail'`

func scanSchedulePass(sc interface{ Scan(...any) error }) (SchedulePass, error) {
	var p SchedulePass
	var startAz, azMax, gain, maxSNR, avgSNR, loss sql.NullFloat64
	var recv, exp, gap sql.NullInt64
	var daylight int
	err := sc.Scan(&p.ID, &p.Satellite, &p.StartTS, &p.EndTS, &p.MaxElevation,
		&startAz, &azMax, &p.Direction, &p.State, &p.ErrorText,
		&p.FileBase, &daylight, &gain, &maxSNR, &avgSNR,
		&recv, &exp, &loss, &gap, &p.ThumbPath)
	if err != nil {
		return p, err
	}
	p.Daylight = daylight == 1
	p.StartAzimuth = nullFloat(startAz)
	p.AzimuthAtMax = nullFloat(azMax)
	p.Gain = nullFloat(gain)
	p.MaxSNR = nullFloat(maxSNR)
	p.AvgSNR = nullFloat(avgSNR)
	p.FrameLossPct = nullFloat(loss)
	p.FramesReceived = nullInt(recv)
	p.FramesExpected = nullInt(exp)
	p.LargestFrameGap = nullInt(gap)
	return p, nil
}

func (s *Store) querySchedulePasses(where string, args ...any) ([]SchedulePass, error) {
	rows, err := s.DB.Query("SELECT "+schedulePassCols+schedulePassFrom+" "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SchedulePass
	for rows.Next() {
		p, err := scanSchedulePass(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) querySchedulePass(where string, args ...any) (*SchedulePass, error) {
	row := s.DB.QueryRow("SELECT "+schedulePassCols+schedulePassFrom+" "+where, args...)
	p, err := scanSchedulePass(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PassesStartingAfter lists every non-cancelled pass with start_ts > from,
// chronological — the schedule table (RN2 Pass::getList, from local
// midnight). Cancelled passes are hidden: RN2 deleted them outright.
func (s *Store) PassesStartingAfter(from time.Time) ([]SchedulePass, error) {
	return s.querySchedulePasses(`WHERE p.start_ts > ? AND p.state != ? ORDER BY p.start_ts`,
		from.Unix(), StateCancelled)
}

// PassesEndingAfter lists non-cancelled passes still ahead or in progress
// (RN2 /api/passes).
func (s *Store) PassesEndingAfter(now time.Time) ([]SchedulePass, error) {
	return s.querySchedulePasses(`WHERE p.end_ts > ? AND p.state != ? ORDER BY p.start_ts`,
		now.Unix(), StateCancelled)
}

// PassByID fetches one pass in any state; nil when absent.
func (s *Store) PassByID(id int64) (*SchedulePass, error) {
	return s.querySchedulePass(`WHERE p.id = ?`, id)
}

// currentCaptureWindow bounds how old a capturing/processing row may be to
// count as live, so a stale state can never show as an active capture.
const currentCaptureWindow = 2 * time.Hour

// CurrentCapture is the pass being captured or processed right now, or nil.
func (s *Store) CurrentCapture(now time.Time) (*SchedulePass, error) {
	return s.querySchedulePass(`WHERE p.state IN (?, ?) AND p.start_ts > ? ORDER BY p.start_ts DESC LIMIT 1`,
		StateCapturing, StateProcessing, now.Add(-currentCaptureWindow).Unix())
}

// NextPass is the next scheduled pass that has not started, or nil.
func (s *Store) NextPass(now time.Time) (*SchedulePass, error) {
	return s.querySchedulePass(`WHERE p.state = ? AND p.start_ts > ? ORDER BY p.start_ts LIMIT 1`,
		StateScheduled, now.Unix())
}

// LatestCapture is the most recent decoded pass, or nil.
func (s *Store) LatestCapture() (*SchedulePass, error) {
	return s.querySchedulePass(`WHERE p.state = ? ORDER BY p.start_ts DESC LIMIT 1`, StateDecoded)
}

// CaptureFilter narrows the gallery (RN2 Capture::filterClause).
type CaptureFilter struct {
	Satellite    string // "" = all
	DayNight     string // "day" | "night" | ""
	MinElevation int    // 0 = any
}

func (f CaptureFilter) clause() (string, []any) {
	where := []string{"p.state = ?"}
	args := []any{StateDecoded}
	if f.Satellite != "" {
		where = append(where, "p.satellite = ?")
		args = append(args, f.Satellite)
	}
	switch f.DayNight {
	case "day":
		where = append(where, "p.daylight = 1")
	case "night":
		where = append(where, "p.daylight = 0")
	}
	if f.MinElevation > 0 {
		where = append(where, "p.max_elevation >= ?")
		args = append(args, f.MinElevation)
	}
	return "WHERE " + strings.Join(where, " AND "), args
}

// Captures pages through decoded passes, newest first.
func (s *Store) Captures(f CaptureFilter, limit, offset int) ([]SchedulePass, error) {
	where, args := f.clause()
	args = append(args, limit, offset)
	return s.querySchedulePasses(where+" ORDER BY p.start_ts DESC LIMIT ? OFFSET ?", args...)
}

// CountCaptures counts decoded passes matching the filter.
func (s *Store) CountCaptures(f CaptureFilter) (int, error) {
	where, args := f.clause()
	var n int
	err := s.DB.QueryRow("SELECT COUNT(*) FROM passes p "+where, args...).Scan(&n)
	return n, err
}

// CaptureSatellites lists the distinct satellite names with captures, for
// the gallery filter dropdown.
func (s *Store) CaptureSatellites() ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT satellite FROM passes WHERE state = ? ORDER BY satellite`, StateDecoded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CancelPass takes a planned pass out of the schedule. Only passes that have
// not run may be cancelled; the cancelled state survives replans (see
// ReplaceFuturePlan), which is what makes this stick where RN2's row delete
// was undone by the next schedule.sh run.
func (s *Store) CancelPass(id int64) error {
	res, err := s.DB.Exec(`UPDATE passes SET state = ?, error_text = 'cancelled from admin page',
			updated_ts = strftime('%s','now')
		WHERE id = ? AND state IN (?, ?)`, StateCancelled, id, StateScheduled, StateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pass %d is not a planned pass", id)
	}
	return nil
}

// DeletePass removes a pass row; its images rows go with it (ON DELETE
// CASCADE). The caller deletes the files first.
func (s *Store) DeletePass(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM passes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pass %d not found", id)
	}
	return nil
}

func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}
