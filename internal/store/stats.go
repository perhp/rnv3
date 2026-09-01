package store

import (
	"database/sql"
	"time"
)

// DayCount is one day of the station record chart: decoded vs failed.
type DayCount struct {
	Day     time.Time // local midnight
	Decoded int
	Failed  int
}

// DailyRecord returns one zero-filled entry per day of the trailing window
// ending today (local time), oldest first. Days are bucketed in Go against
// the process time zone rather than in SQL so the boundaries match what the
// panel prints.
func (s *Store) DailyRecord(days int, now time.Time) ([]DayCount, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	first := today.AddDate(0, 0, -(days - 1))
	out := make([]DayCount, days)
	index := map[string]int{}
	for i := range out {
		d := first.AddDate(0, 0, i)
		out[i].Day = d
		index[d.Format("2006-01-02")] = i
	}
	rows, err := s.DB.Query(`SELECT start_ts, state FROM passes WHERE start_ts >= ? AND state IN (?, ?)`,
		first.Unix(), StateDecoded, StateFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var state string
		if err := rows.Scan(&ts, &state); err != nil {
			return nil, err
		}
		key := time.Unix(ts, 0).In(now.Location()).Format("2006-01-02")
		i, ok := index[key]
		if !ok {
			continue
		}
		if state == StateDecoded {
			out[i].Decoded++
		} else {
			out[i].Failed++
		}
	}
	return out, rows.Err()
}

// SatelliteStats aggregates one satellite's capture history.
type SatelliteStats struct {
	Satellite    string
	Captures     int
	AvgElevation float64
	AvgSNR       *float64
	BestSNR      *float64
	AvgFrameLoss *float64
	LastCapture  int64
}

// PerSatellite aggregates decoded passes per satellite, most captures first.
func (s *Store) PerSatellite() ([]SatelliteStats, error) {
	rows, err := s.DB.Query(`SELECT satellite, COUNT(*), AVG(max_elevation), AVG(max_snr), MAX(max_snr),
			AVG(frame_loss_pct), MAX(start_ts)
		FROM passes WHERE state = ? GROUP BY satellite ORDER BY COUNT(*) DESC, satellite`, StateDecoded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SatelliteStats
	for rows.Next() {
		var st SatelliteStats
		var avgSNR, bestSNR, avgLoss sql.NullFloat64
		if err := rows.Scan(&st.Satellite, &st.Captures, &st.AvgElevation, &avgSNR, &bestSNR, &avgLoss, &st.LastCapture); err != nil {
			return nil, err
		}
		st.AvgSNR = nullFloat(avgSNR)
		st.BestSNR = nullFloat(bestSNR)
		st.AvgFrameLoss = nullFloat(avgLoss)
		out = append(out, st)
	}
	return out, rows.Err()
}

// Totals are the station-level counters on the stats page.
type Totals struct {
	TotalCaptures int
	Attempted30d  int // passes that ran (decoded or failed) in the last 30 days
	Failed30d     int
	FirstCapture  *int64
	PassesPlotted int // passes on the sky map
}

// StationTotals computes the counters as of now.
func (s *Store) StationTotals(now time.Time) (Totals, error) {
	var t Totals
	cutoff := now.Add(-30 * 24 * time.Hour).Unix()
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM passes WHERE state = ?`, StateDecoded).Scan(&t.TotalCaptures); err != nil {
		return t, err
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM passes WHERE state IN (?, ?) AND start_ts > ? AND start_ts < ?`,
		StateDecoded, StateFailed, cutoff, now.Unix()).Scan(&t.Attempted30d); err != nil {
		return t, err
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM passes WHERE state = ? AND start_ts > ? AND start_ts < ?`,
		StateFailed, cutoff, now.Unix()).Scan(&t.Failed30d); err != nil {
		return t, err
	}
	var first sql.NullInt64
	if err := s.DB.QueryRow(`SELECT MIN(start_ts) FROM passes WHERE state = ?`, StateDecoded).Scan(&first); err != nil {
		return t, err
	}
	if first.Valid {
		v := first.Int64
		t.FirstCapture = &v
	}
	// Same predicate as SkymapPoints, so the caption matches the plot.
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM passes WHERE state IN (?, ?) AND azimuth_at_max IS NOT NULL`,
		StateDecoded, StateFailed).Scan(&t.PassesPlotted); err != nil {
		return t, err
	}
	return t, nil
}
