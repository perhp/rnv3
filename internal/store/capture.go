package store

import "time"

// CaptureResult is everything a finished decode writes back to the pass row.
type CaptureResult struct {
	FileBase string
	Daylight bool
	Gain     float64
	// SNR values are pointers: nil = no readings seen (stored as NULL, RN2 parity).
	MaxSNR *float64
	AvgSNR *float64
}

// CompleteCapture marks a pass decoded and stores its capture metadata.
func (s *Store) CompleteCapture(id int64, r CaptureResult) error {
	daylight := 0
	if r.Daylight {
		daylight = 1
	}
	_, err := s.DB.Exec(`UPDATE passes SET
			state = ?, error_text = NULL, file_base = ?, daylight = ?, gain = ?,
			max_snr = ?, avg_snr = ?, updated_ts = strftime('%s','now')
		WHERE id = ?`,
		StateDecoded, r.FileBase, daylight, r.Gain,
		nullableFloat(r.MaxSNR), nullableFloat(r.AvgSNR), id)
	return err
}

// SetFrameStats records CADU frame-yield metrics. They live on the pass row
// (not on a decode result) deliberately: a pass that decoded nothing is
// exactly where frame yield matters most.
func (s *Store) SetFrameStats(id int64, received, expected int, lossPct float64, largestGap int) error {
	_, err := s.DB.Exec(`UPDATE passes SET
			frames_received = ?, frames_expected = ?, frame_loss_pct = ?, largest_frame_gap = ?,
			updated_ts = strftime('%s','now')
		WHERE id = ?`,
		received, expected, lossPct, largestGap, id)
	return err
}

// MarkMissedScheduled sweeps passes that were never captured (daemon down
// during their window) out of the scheduled state so the plan stays honest.
func (s *Store) MarkMissedScheduled(now time.Time) (int64, error) {
	res, err := s.DB.Exec(`UPDATE passes SET
			state = ?, error_text = 'missed (station was not running)', updated_ts = strftime('%s','now')
		WHERE state = ? AND end_ts < ?`,
		StateSkipped, StateScheduled, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullableFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
