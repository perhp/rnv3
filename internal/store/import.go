package store

import "database/sql"

// ImportedPass is a historical pass carried over from RN2's panel.db. ID,
// when non-zero, is inserted explicitly so old capture URLs keep resolving.
type ImportedPass struct {
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
	Daylight        *bool
	Gain            *float64
	MaxSNR          *float64
	AvgSNR          *float64
	FramesReceived  *int
	FramesExpected  *int
	FrameLossPct    *float64
	LargestFrameGap *int
}

// InsertImported adds a historical pass inside tx. Returns the row id and
// false when a pass with the same satellite/start already exists (re-running
// the importer is safe), true when inserted.
func (s *Store) InsertImported(tx *sql.Tx, p ImportedPass) (int64, bool, error) {
	var existing int64
	err := tx.QueryRow(`SELECT id FROM passes WHERE satellite = ? AND start_ts = ?`, p.Satellite, p.StartTS).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	var id any
	if p.ID > 0 {
		id = p.ID
	}
	var daylight any
	if p.Daylight != nil {
		daylight = 0
		if *p.Daylight {
			daylight = 1
		}
	}
	res, err := tx.Exec(`INSERT INTO passes (id, satellite, start_ts, end_ts, max_elevation, start_azimuth,
			azimuth_at_max, direction, state, error_text, file_base, daylight, gain, max_snr, avg_snr,
			frames_received, frames_expected, frame_loss_pct, largest_frame_gap)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.Satellite, p.StartTS, p.EndTS, p.MaxElevation, nullableFloat(p.StartAzimuth),
		nullableFloat(p.AzimuthAtMax), nullIfEmpty(p.Direction), p.State, nullIfEmpty(p.ErrorText),
		nullIfEmpty(p.FileBase), daylight, nullableFloat(p.Gain), nullableFloat(p.MaxSNR), nullableFloat(p.AvgSNR),
		nullableInt(p.FramesReceived), nullableInt(p.FramesExpected), nullableFloat(p.FrameLossPct), nullableInt(p.LargestFrameGap))
	if err != nil {
		return 0, false, err
	}
	newID, err := res.LastInsertId()
	return newID, true, err
}

// PassKey identifies a pass independently of its row id.
type PassKey struct {
	Satellite string
	StartTS   int64
}

// PassIDs maps every existing pass id to its identity (importer collision
// check).
func (s *Store) PassIDs(tx *sql.Tx) (map[int64]PassKey, error) {
	rows, err := tx.Query(`SELECT id, satellite, start_ts FROM passes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]PassKey{}
	for rows.Next() {
		var id int64
		var k PassKey
		if err := rows.Scan(&id, &k.Satellite, &k.StartTS); err != nil {
			return nil, err
		}
		out[id] = k
	}
	return out, rows.Err()
}

// RenumberPass moves a pass (and its images) to a new id inside tx. Foreign
// key checks are deferred to commit so the images rows can follow.
func (s *Store) RenumberPass(tx *sql.Tx, oldID, newID int64) error {
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE passes SET id = ? WHERE id = ?`, newID, oldID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE images SET pass_id = ? WHERE pass_id = ?`, newID, oldID)
	return err
}

// AddImageTx is AddImage inside a transaction (importer).
func (s *Store) AddImageTx(tx *sql.Tx, passID int64, kind, path, thumbPath string) error {
	_, err := tx.Exec(`INSERT INTO images (pass_id, kind, path, thumb_path) VALUES (?, ?, ?, ?)`,
		passID, kind, path, nullIfEmpty(thumbPath))
	return err
}

func nullableInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}
