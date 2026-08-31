package store

import (
	"database/sql"
	"time"
)

// Image is one produced artifact of a pass.
type Image struct {
	ID        int64
	PassID    int64
	Kind      string // enhancement name, "polar-azel", "polar-direction", "website-thumbnail"
	Path      string // filename relative to paths.images
	ThumbPath string // filename relative to paths.thumbs, "" if none
}

// AddImage registers a produced image for a pass.
func (s *Store) AddImage(passID int64, kind, path, thumbPath string) error {
	_, err := s.DB.Exec(`INSERT INTO images (pass_id, kind, path, thumb_path) VALUES (?, ?, ?, ?)`,
		passID, kind, path, nullIfEmpty(thumbPath))
	return err
}

// ImagesForPass lists a pass's images in insertion order.
func (s *Store) ImagesForPass(passID int64) ([]Image, error) {
	rows, err := s.DB.Query(`SELECT id, pass_id, kind, path, COALESCE(thumb_path, '')
		FROM images WHERE pass_id = ? ORDER BY id`, passID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var im Image
		if err := rows.Scan(&im.ID, &im.PassID, &im.Kind, &im.Path, &im.ThumbPath); err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// DecodedPass is the slice of a pass row the daily-artifact builders need.
type DecodedPass struct {
	ID       int64
	FileBase string
	MaxSNR   *float64
	Daylight bool
}

// DecodedPassesBetween returns decoded passes with start_ts in [from, to),
// chronological — the frame source for mosaics and timelapses.
func (s *Store) DecodedPassesBetween(from, to time.Time) ([]DecodedPass, error) {
	rows, err := s.DB.Query(`SELECT id, COALESCE(file_base, ''), max_snr, COALESCE(daylight, 0)
		FROM passes WHERE state = ? AND start_ts >= ? AND start_ts < ? ORDER BY start_ts`,
		StateDecoded, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecodedPass
	for rows.Next() {
		var p DecodedPass
		var snr sql.NullFloat64
		var daylight int
		if err := rows.Scan(&p.ID, &p.FileBase, &snr, &daylight); err != nil {
			return nil, err
		}
		if snr.Valid {
			v := snr.Float64
			p.MaxSNR = &v
		}
		p.Daylight = daylight == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// SkymapPoint is one historical pass for the reception-quality sky map.
type SkymapPoint struct {
	AzimuthAtMax float64
	MaxElevation float64
	MaxSNR       *float64
	Failed       bool
}

// SkymapPoints returns every finished pass with a recorded max-elevation
// azimuth: decoded ones carry SNR when known, failed ones plot as failures.
func (s *Store) SkymapPoints() ([]SkymapPoint, error) {
	rows, err := s.DB.Query(`SELECT azimuth_at_max, max_elevation, max_snr, state
		FROM passes WHERE state IN (?, ?) AND azimuth_at_max IS NOT NULL`,
		StateDecoded, StateFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkymapPoint
	for rows.Next() {
		var p SkymapPoint
		var snr sql.NullFloat64
		var state string
		if err := rows.Scan(&p.AzimuthAtMax, &p.MaxElevation, &snr, &state); err != nil {
			return nil, err
		}
		if snr.Valid {
			v := snr.Float64
			p.MaxSNR = &v
		}
		p.Failed = state == StateFailed
		out = append(out, p)
	}
	return out, rows.Err()
}
