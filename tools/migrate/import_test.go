package main

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/store"
)

// rn2Schema is RN2's db_migrations/00..12 applied in order — the exact
// shape of a current panel.db.
const rn2Schema = `
CREATE TABLE IF NOT EXISTS predict_passes(
    sat_name text not null,
    pass_start timestamp primary key default (strftime('%s', 'now')) not null,
    pass_end timestamp default (strftime('%s', 'now')) not null,
    max_elev int not null,
    is_active boolean);
CREATE TABLE IF NOT EXISTS decoded_passes(
    id integer primary key autoincrement,
    pass_start integer,
    file_path text not null,
    daylight_pass boolean, is_noaa boolean, sat_type integer, img_count integer,
    foreign key(pass_start) references passes(pass_start));
ALTER TABLE decoded_passes ADD COLUMN has_spectrogram boolean default 0;
ALTER TABLE decoded_passes ADD COLUMN has_pristine boolean default 0;
ALTER TABLE predict_passes ADD COLUMN pass_start_azimuth int;
ALTER TABLE predict_passes ADD COLUMN direction text;
ALTER TABLE decoded_passes ADD COLUMN gain real;
ALTER TABLE predict_passes ADD COLUMN azimuth_at_max int;
ALTER TABLE decoded_passes ADD COLUMN has_polar_az_el boolean default 0;
ALTER TABLE decoded_passes ADD COLUMN has_polar_direction boolean default 0;
ALTER TABLE decoded_passes ADD COLUMN has_histogram boolean default 0;
ALTER TABLE predict_passes ADD COLUMN at_job_id int not null default 0;
ALTER TABLE predict_passes ADD COLUMN status text;
ALTER TABLE predict_passes ADD COLUMN error_text text;
ALTER TABLE decoded_passes ADD COLUMN max_snr real;
ALTER TABLE decoded_passes ADD COLUMN avg_snr real;
ALTER TABLE predict_passes ADD COLUMN frames_received integer;
ALTER TABLE predict_passes ADD COLUMN frames_expected integer;
ALTER TABLE predict_passes ADD COLUMN frame_loss_pct real;
ALTER TABLE predict_passes ADD COLUMN largest_frame_gap integer;
`

func openOld(t *testing.T, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func touch(t *testing.T, path string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportMapsStatesImagesAndIDs(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	old := openOld(t, rn2Schema)
	exec := func(q string, args ...any) {
		if _, err := old.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	past := now.Unix() - 86400
	// decoded meteor pass with frame stats
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active, pass_start_azimuth, direction, azimuth_at_max, status,
		frames_received, frames_expected, frame_loss_pct, largest_frame_gap) VALUES ('METEOR-M2 3', ?, ?, 71, 0, 12, 'Northbound', 95, 'completed', 900, 1000, 10.0, 30)`, past, past+700)
	exec(`INSERT INTO decoded_passes (id, pass_start, file_path, daylight_pass, sat_type, gain, max_snr, avg_snr) VALUES (4711, ?, 'METEOR-M2-3-20260831-120000', 1, 0, 40.2, 11.5, 7.2)`, past)
	// decoded NOAA pass, night
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active, direction, azimuth_at_max) VALUES ('NOAA 19', ?, ?, 45, 0, 'Southbound', 250)`, past+5000, past+5800)
	exec(`INSERT INTO decoded_passes (id, pass_start, file_path, daylight_pass, sat_type, gain) VALUES (4712, ?, 'NOAA-19-20260831-132320', 0, 1, 0)`, past+5000)
	// failed
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active, status, error_text) VALUES ('NOAA 18', ?, ?, 33, 0, 'failed', 'no wav')`, past+9000, past+9600)
	// stale capturing
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active, status) VALUES ('NOAA 15', ?, ?, 33, 1, 'capturing')`, past+12000, past+12600)
	// conflict loser in the past (is_active 0, no status)
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active) VALUES ('METEOR-M2 4', ?, ?, 20, 0)`, past+12100, past+12700)
	// missed (active, past, never ran)
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active) VALUES ('NOAA 19', ?, ?, 50, 1)`, past+20000, past+20600)
	// future active + future inactive
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active) VALUES ('NOAA 19', ?, ?, 60, 1)`, now.Unix()+3600, now.Unix()+4200)
	exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active) VALUES ('NOAA 18', ?, ?, 25, 0)`, now.Unix()+3700, now.Unix()+4300)

	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	thumbs := filepath.Join(images, "thumb")
	for _, name := range []string{"METEOR-M2-3-20260831-120000-221_projected.jpg", "METEOR-M2-3-20260831-120000-MSA_corrected.jpg",
		"METEOR-M2-3-20260831-120000-polar-azel.jpg", "METEOR-M2-3-20260831-120000-polar-direction.png",
		"METEOR-M2-3-20260831-120000.cadu", "METEOR-M2-3-20260831-120000-notes.txt",
		"METEOR-M2-30-decoy-MCIR.jpg", "NOAA-19-20260831-132320-MCIR.jpg"} {
		touch(t, filepath.Join(images, name))
	}
	for _, name := range []string{"METEOR-M2-3-20260831-120000-221_projected.jpg", "METEOR-M2-3-20260831-120000-website-thumbnail.jpg",
		"NOAA-19-20260831-132320-MCIR.jpg"} {
		touch(t, filepath.Join(thumbs, name))
	}

	st, err := store.Open(filepath.Join(dir, "rnv3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sum, err := Import(old, st, images, thumbs, now, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Passes != 8 || sum.Inserted != 8 || sum.Decoded != 2 || sum.Images != 5 || sum.Thumbs != 2 || sum.Missing != 0 {
		t.Errorf("summary = %+v", sum)
	}

	// Old capture ids survive, so bookmarked capture URLs keep working.
	meteor, _ := st.PassByID(4711)
	if meteor == nil || meteor.State != store.StateDecoded || meteor.FileBase != "METEOR-M2-3-20260831-120000" ||
		meteor.Direction != "northbound" || meteor.FramesReceived == nil || *meteor.FramesReceived != 900 ||
		meteor.MaxSNR == nil || *meteor.MaxSNR != 11.5 || !meteor.Daylight || meteor.ThumbPath != "METEOR-M2-3-20260831-120000-website-thumbnail.jpg" {
		t.Errorf("meteor capture = %+v", meteor)
	}
	imgs, _ := st.ImagesForPass(4711)
	kinds := map[string]store.Image{}
	for _, im := range imgs {
		kinds[im.Kind] = im
	}
	if len(imgs) != 5 { // 4 imagery/plots + website thumbnail row
		t.Errorf("images = %+v", imgs)
	}
	if im := kinds["221_projected"]; im.Path != "METEOR-M2-3-20260831-120000-221_projected.jpg" || im.ThumbPath != im.Path {
		t.Errorf("221_projected = %+v", im)
	}
	if im := kinds["MSA_corrected"]; im.ThumbPath != "" {
		t.Errorf("MSA_corrected should have no thumb: %+v", im)
	}
	if _, ok := kinds["polar-direction"]; !ok {
		t.Error("polar-direction png not registered")
	}
	if im := kinds["website-thumbnail"]; im.Path != "" || im.ThumbPath == "" {
		t.Errorf("website thumbnail row = %+v", im)
	}
	for _, bad := range []string{"", "notes", "decoy-MCIR"} {
		if _, ok := kinds[bad]; ok {
			t.Errorf("kind %q must not be registered", bad)
		}
	}
	noaa, _ := st.PassByID(4712)
	if noaa == nil || noaa.Daylight || noaa.Gain == nil || *noaa.Gain != 0 || noaa.ThumbPath != "" {
		t.Errorf("noaa capture = %+v", noaa)
	}

	states := map[string]string{}
	rows, _ := st.DB.Query(`SELECT satellite || '@' || start_ts, state || '|' || COALESCE(error_text,'') FROM passes`)
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		states[k] = v
	}
	rows.Close()
	key := func(sat string, ts int64) string { return sat + "@" + itoa(ts) }
	want := map[string]string{
		key("NOAA 18", past+9000):       "failed|no wav",
		key("NOAA 15", past+12000):      "failed|interrupted (imported from RN2 mid-capturing)",
		key("METEOR-M2 4", past+12100):  "skipped|not scheduled (imported from RN2)",
		key("NOAA 19", past+20000):      "skipped|not captured (imported from RN2)",
		key("NOAA 19", now.Unix()+3600): "scheduled|",
		key("NOAA 18", now.Unix()+3700): "skipped|not scheduled (imported from RN2)",
	}
	for k, v := range want {
		if states[k] != v {
			t.Errorf("%s: state %q, want %q", k, states[k], v)
		}
	}

	// Re-running is a no-op.
	sum2, err := Import(old, st, images, thumbs, now, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.Inserted != 0 || sum2.Skipped != 8 {
		t.Errorf("second run = %+v", sum2)
	}
	if imgs, _ := st.ImagesForPass(4711); len(imgs) != 5 {
		t.Errorf("images duplicated on re-run: %d", len(imgs))
	}
	points, _ := st.SkymapPoints()
	if len(points) != 2 { // the two rows carrying azimuth_at_max
		t.Errorf("skymap points = %d", len(points))
	}
}

func TestImportToleratesOldSchema(t *testing.T) {
	// A panel.db from before the azimuth/status/SNR migrations.
	old := openOld(t, `
CREATE TABLE predict_passes(sat_name text not null, pass_start timestamp primary key not null, pass_end timestamp not null, max_elev int not null, is_active boolean);
CREATE TABLE decoded_passes(id integer primary key autoincrement, pass_start integer, file_path text not null, daylight_pass boolean, is_noaa boolean, sat_type integer, img_count integer);
INSERT INTO predict_passes VALUES ('NOAA 15', 1500000000, 1500000600, 30, 0);
INSERT INTO predict_passes VALUES ('NOAA 18', 1550000000, 1550000600, 30, 1);
INSERT INTO predict_passes VALUES ('NOAA 19', 1600000000, 1600000600, 55, 0);
INSERT INTO decoded_passes (id, pass_start, file_path, daylight_pass) VALUES (1, 1600000000, 'NOAA-19-20200913-122640', 1);
`)
	st, err := store.Open(filepath.Join(t.TempDir(), "rnv3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sum, err := Import(old, st, filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "none"), time.Now(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Inserted != 3 || sum.Decoded != 1 || sum.Missing != 1 {
		t.Errorf("summary = %+v", sum)
	}
	// The capture keeps id 1 even though two older passes were imported
	// before it in time — auto ids must never collide with preserved ones.
	p, _ := st.PassByID(1)
	if p == nil || p.State != store.StateDecoded || p.Satellite != "NOAA 19" || p.AzimuthAtMax != nil || p.Gain != nil {
		t.Errorf("pass = %+v", p)
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM passes WHERE id > 1`).Scan(&n)
	if n != 2 {
		t.Errorf("non-decoded passes got ids: %d rows above id 1, want 2", n)
	}
}

// TestImportRenumbersCollidingDestinationRows: the daemon may have planned
// or captured passes before the one-time import; their auto ids must give
// way to RN2's capture ids instead of failing the whole import.
func TestImportRenumbersCollidingDestinationRows(t *testing.T) {
	old := openOld(t, rn2Schema)
	if _, err := old.Exec(`INSERT INTO predict_passes (sat_name, pass_start, pass_end, max_elev, is_active) VALUES ('NOAA 19', 1600000000, 1600000600, 55, 0);
		INSERT INTO decoded_passes (id, pass_start, file_path, daylight_pass) VALUES (2, 1600000000, 'NOAA-19-20200913-122640', 1);`); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "rnv3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Destination already holds ids 1..3: id 2 is a different pass with an image.
	tx, _ := st.DB.Begin()
	for i, ts := range []int64{1700000000, 1700001000, 1700002000} {
		st.InsertImported(tx, store.ImportedPass{Satellite: "METEOR-M2 3", StartTS: ts, EndTS: ts + 600, MaxElevation: 40, State: store.StateDecoded, FileBase: "M" + itoa(int64(i))})
	}
	tx.Commit()
	st.AddImage(2, "MCIR", "M1-MCIR.jpg", "")

	sum, err := Import(old, st, filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "none"), time.Now(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Inserted != 1 {
		t.Errorf("summary = %+v", sum)
	}
	p, _ := st.PassByID(2)
	if p == nil || p.Satellite != "NOAA 19" || p.FileBase != "NOAA-19-20200913-122640" {
		t.Fatalf("RN2 capture did not get id 2: %+v", p)
	}
	moved, _ := st.PassByID(4) // above every id in either database
	if moved == nil || moved.FileBase != "M1" {
		t.Fatalf("displaced pass = %+v", moved)
	}
	if imgs, _ := st.ImagesForPass(4); len(imgs) != 1 || imgs[0].Path != "M1-MCIR.jpg" {
		t.Errorf("images did not follow the renumbered pass: %+v", imgs)
	}
	var n int
	st.DB.QueryRow(`SELECT COUNT(*) FROM passes`).Scan(&n)
	if n != 4 {
		t.Errorf("%d passes, want 4", n)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
