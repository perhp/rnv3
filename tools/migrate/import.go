package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/store"
)

// Summary counts what the import did.
type Summary struct {
	Passes   int // predict_passes rows seen
	Inserted int
	Skipped  int // already present (re-run)
	Decoded  int
	Images   int
	Thumbs   int
	Missing  int // decoded passes with no image on disk
}

func (s Summary) String() string {
	return fmt.Sprintf("passes: %d seen, %d inserted, %d already present; captures: %d (%d images, %d thumbnails, %d captures without images on disk)",
		s.Passes, s.Inserted, s.Skipped, s.Decoded, s.Images, s.Thumbs, s.Missing)
}

// optional lists predict_passes/decoded_passes columns added by RN2's later
// migrations; a panel.db that predates one simply yields NULL for it.
var optional = map[string]bool{
	"pass_start_azimuth": true, "azimuth_at_max": true, "direction": true, "status": true, "error_text": true,
	"frames_received": true, "frames_expected": true, "frame_loss_pct": true, "largest_frame_gap": true,
	"gain": true, "max_snr": true, "avg_snr": true,
}

func columns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// col renders a select expression for an optional column.
func col(have map[string]bool, alias, name string) string {
	if have[name] {
		return alias + "." + name
	}
	if !optional[name] {
		panic("required column missing: " + name)
	}
	return "NULL AS " + name
}

// imageExts are what the gallery showed (RN2 Capture::getEnhancements) plus
// rnv3's SVG plots.
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".svg": true}

// Import copies RN2's predict_passes/decoded_passes into the rnv3 store and
// registers the capture imagery found on disk. Idempotent: passes already
// present (same satellite + start) are left alone.
func Import(old *sql.DB, st *store.Store, imagesDir, thumbsDir string, now time.Time, log io.Writer) (Summary, error) {
	var sum Summary
	pcols, err := columns(old, "predict_passes")
	if err != nil {
		return sum, fmt.Errorf("read predict_passes schema: %w", err)
	}
	if len(pcols) == 0 {
		return sum, fmt.Errorf("predict_passes table not found — is this an RN2 panel.db?")
	}
	dcols, err := columns(old, "decoded_passes")
	if err != nil {
		return sum, err
	}
	for _, need := range []string{"sat_name", "pass_start", "pass_end", "max_elev", "is_active"} {
		if !pcols[need] {
			return sum, fmt.Errorf("predict_passes lacks column %s", need)
		}
	}

	query := `SELECT p.sat_name, p.pass_start, p.pass_end, p.max_elev, COALESCE(p.is_active, 0),
		` + col(pcols, "p", "pass_start_azimuth") + `, ` + col(pcols, "p", "azimuth_at_max") + `, ` + col(pcols, "p", "direction") + `,
		` + col(pcols, "p", "status") + `, ` + col(pcols, "p", "error_text") + `,
		` + col(pcols, "p", "frames_received") + `, ` + col(pcols, "p", "frames_expected") + `,
		` + col(pcols, "p", "frame_loss_pct") + `, ` + col(pcols, "p", "largest_frame_gap") + `,
		d.id, d.file_path, d.daylight_pass, ` + col(dcols, "d", "gain") + `, ` + col(dcols, "d", "max_snr") + `, ` + col(dcols, "d", "avg_snr") + `
		FROM predict_passes p LEFT JOIN decoded_passes d ON d.pass_start = p.pass_start
		ORDER BY (d.id IS NULL), p.pass_start`
	// Decoded passes go first: they keep their RN2 ids, and SQLite's
	// AUTOINCREMENT then hands the remaining passes ids above the largest
	// one inserted, so an auto id can never collide with a preserved one.
	rows, err := old.Query(query)
	if err != nil {
		return sum, fmt.Errorf("read old passes: %w", err)
	}
	defer rows.Close()

	images, err := listNames(imagesDir)
	if err != nil {
		return sum, err
	}
	thumbs, err := listNames(thumbsDir)
	if err != nil {
		return sum, err
	}
	thumbSet := map[string]bool{}
	for _, t := range thumbs {
		thumbSet[t] = true
	}

	tx, err := st.DB.Begin()
	if err != nil {
		return sum, err
	}
	defer tx.Rollback()

	if err := clearIDCollisions(old, st, tx, log); err != nil {
		return sum, err
	}

	for rows.Next() {
		var (
			p                                             store.ImportedPass
			satName, direction, status, errText           sql.NullString
			startAz, azMax, loss, gain, maxSNR, avgSNR    sql.NullFloat64
			isActive, recv, exp, gap, decodedID, daylight sql.NullInt64
			filePath                                      sql.NullString
		)
		if err := rows.Scan(&satName, &p.StartTS, &p.EndTS, &p.MaxElevation, &isActive,
			&startAz, &azMax, &direction, &status, &errText, &recv, &exp, &loss, &gap,
			&decodedID, &filePath, &daylight, &gain, &maxSNR, &avgSNR); err != nil {
			return sum, err
		}
		sum.Passes++
		p.Satellite = satName.String
		p.StartAzimuth = optFloat(startAz)
		p.AzimuthAtMax = optFloat(azMax)
		p.Direction = strings.ToLower(direction.String)
		p.FramesReceived = optInt(recv)
		p.FramesExpected = optInt(exp)
		p.FrameLossPct = optFloat(loss)
		p.LargestFrameGap = optInt(gap)

		decoded := decodedID.Valid
		switch {
		case decoded:
			p.ID = decodedID.Int64 // keep RN2's capture id so old URLs resolve
			p.State = store.StateDecoded
			p.FileBase = filePath.String
			d := daylight.Valid && daylight.Int64 == 1
			p.Daylight = &d
			p.Gain = optFloat(gain)
			p.MaxSNR = optFloat(maxSNR)
			p.AvgSNR = optFloat(avgSNR)
		case status.String == "failed":
			p.State = store.StateFailed
			p.ErrorText = errText.String
		case status.String == "capturing" || status.String == "processing":
			p.State = store.StateFailed
			p.ErrorText = "interrupted (imported from RN2 mid-" + status.String + ")"
		case p.EndTS < now.Unix():
			p.State = store.StateSkipped
			if isActive.Int64 == 1 {
				p.ErrorText = "not captured (imported from RN2)"
			} else {
				p.ErrorText = "not scheduled (imported from RN2)"
			}
		case isActive.Int64 == 1:
			p.State = store.StateScheduled // the scheduler replans it anyway
		default:
			p.State = store.StateSkipped
			p.ErrorText = "not scheduled (imported from RN2)"
		}

		id, inserted, err := st.InsertImported(tx, p)
		if err != nil {
			return sum, fmt.Errorf("pass %s @ %d: %w", p.Satellite, p.StartTS, err)
		}
		if !inserted {
			sum.Skipped++
			continue
		}
		sum.Inserted++
		if !decoded {
			continue
		}
		sum.Decoded++
		n, nt, err := registerImages(st, tx, id, p.FileBase, images, thumbSet)
		if err != nil {
			return sum, err
		}
		if n == 0 {
			sum.Missing++
			fmt.Fprintf(log, "warning: no images on disk for %s (capture %d)\n", p.FileBase, id)
		}
		sum.Images += n
		sum.Thumbs += nt
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}
	if err := tx.Commit(); err != nil {
		return sum, err
	}
	return sum, nil
}

// clearIDCollisions renumbers destination passes whose auto-assigned id
// equals an RN2 capture id belonging to a different pass, so the capture can
// keep its id. Happens when the daemon has already planned or captured
// passes before the one-time import. Passes are moved above every id in
// either database, which also keeps later auto ids clear.
func clearIDCollisions(old *sql.DB, st *store.Store, tx *sql.Tx, log io.Writer) error {
	existing, err := st.PassIDs(tx)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}
	rows, err := old.Query(`SELECT d.id, p.sat_name, d.pass_start FROM decoded_passes d
		JOIN predict_passes p ON p.pass_start = d.pass_start`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var next int64
	for id := range existing {
		if id > next {
			next = id
		}
	}
	type move struct{ from int64 }
	var moves []move
	for rows.Next() {
		var id int64
		var key store.PassKey
		if err := rows.Scan(&id, &key.Satellite, &key.StartTS); err != nil {
			return err
		}
		if id > next {
			next = id
		}
		if cur, taken := existing[id]; taken && cur != key {
			moves = append(moves, move{from: id})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range moves {
		next++
		if err := st.RenumberPass(tx, m.from, next); err != nil {
			return fmt.Errorf("renumber pass %d → %d: %w", m.from, next, err)
		}
		fmt.Fprintf(log, "moved existing pass %d to id %d to make room for the RN2 capture with that id\n", m.from, next)
	}
	return nil
}

// registerImages adds an images row for every "<fileBase>-<kind>.<ext>" in
// the images directory, with its thumbnail when one exists, and the website
// thumbnail (thumbs only) the gallery card uses.
func registerImages(st *store.Store, tx *sql.Tx, passID int64, fileBase string, images []string, thumbs map[string]bool) (int, int, error) {
	if fileBase == "" {
		return 0, 0, nil
	}
	prefix := fileBase + "-"
	start := sort.SearchStrings(images, prefix)
	n, nt := 0, 0
	for i := start; i < len(images) && strings.HasPrefix(images[i], prefix); i++ {
		name := images[i]
		ext := strings.ToLower(filepath.Ext(name))
		if !imageExts[ext] {
			continue
		}
		kind := strings.TrimSuffix(strings.TrimPrefix(name, prefix), filepath.Ext(name))
		if kind == "website-thumbnail" || kind == "" {
			continue
		}
		thumb := ""
		if thumbs[name] {
			thumb = name
			nt++
		}
		if err := st.AddImageTx(tx, passID, kind, name, thumb); err != nil {
			return n, nt, err
		}
		n++
	}
	if wt := fileBase + "-website-thumbnail.jpg"; thumbs[wt] {
		if err := st.AddImageTx(tx, passID, "website-thumbnail", "", wt); err != nil {
			return n, nt, err
		}
	}
	return n, nt, nil
}

// listNames returns the sorted file names of dir (empty when dir is absent).
func listNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func optFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func optInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}
