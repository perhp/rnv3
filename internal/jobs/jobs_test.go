package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

type fakeNotifier struct {
	mu        sync.Mutex
	alerts    []string // "check: message"
	summaries []string
	files     [][]string
}

func (f *fakeNotifier) Alert(_ context.Context, check, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, check+": "+message)
}

func (f *fakeNotifier) DailySummary(_ context.Context, annotation string, files []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries = append(f.summaries, annotation)
	f.files = append(f.files, files)
}

func (f *fakeNotifier) checks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.alerts {
		out = append(out, strings.SplitN(a, ":", 2)[0])
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func newJobs(t *testing.T) (*Jobs, *fakeNotifier, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.Paths.Images = filepath.Join(dir, "images")
	cfg.Paths.Thumbs = filepath.Join(dir, "images", "thumb")
	cfg.Paths.Work = filepath.Join(dir, "work")
	cfg.Paths.Ramfs = filepath.Join(dir, "ramfs")
	cfg.Station.Location = "Copenhagen"
	os.MkdirAll(cfg.Paths.Thumbs, 0o755)
	os.MkdirAll(cfg.Paths.Work, 0o755)
	n := &fakeNotifier{}
	return &Jobs{Prov: config.NewProvider(cfg), St: st, Notify: n, StateDir: dir}, n, cfg
}

func insert(t *testing.T, st *store.Store, p store.ImportedPass) int64 {
	t.Helper()
	tx, _ := st.DB.Begin()
	id, _, err := st.InsertImported(tx, p)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	return id
}

func touch(t *testing.T, path string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdogAlertsSuppressesAndClears(t *testing.T) {
	j, n, _ := newJobs(t)
	wd := newWatchdog(j)
	now := time.Now()
	wd.runOnce(context.Background(), now)
	got := n.checks()
	if len(got) != 2 || got[0] != "no_recent_capture" || got[1] != "no_scheduled_passes" {
		t.Fatalf("empty station alerts = %v", got)
	}
	// Same failures an hour later: suppressed.
	wd.runOnce(context.Background(), now.Add(time.Hour))
	if len(n.checks()) != 2 {
		t.Errorf("re-alert within 24h must be suppressed: %v", n.checks())
	}
	// State survives a restart.
	raw, err := os.ReadFile(filepath.Join(j.StateDir, "watchdog-state.json"))
	if err != nil {
		t.Fatal("state file not written")
	}
	var state map[string]int64
	json.Unmarshal(raw, &state)
	if state["no_recent_capture"] != now.Unix() {
		t.Errorf("persisted state = %v", state)
	}
	wd2 := newWatchdog(j)
	wd2.runOnce(context.Background(), now.Add(2*time.Hour))
	if len(n.checks()) != 2 {
		t.Errorf("a restart must not re-alert: %v", n.checks())
	}
	// After 24h the alert repeats.
	wd2.runOnce(context.Background(), now.Add(25*time.Hour))
	if len(n.checks()) != 4 {
		t.Errorf("after 24h the alert must repeat: %v", n.checks())
	}

	// Station recovers: checks clear, and the next failure alerts at once.
	insert(t, j.St, store.ImportedPass{Satellite: "NOAA 19", StartTS: now.Add(26 * time.Hour).Unix(), EndTS: now.Add(26*time.Hour + 600*time.Second).Unix(), MaxElevation: 50, State: store.StateDecoded, FileBase: "x"})
	insert(t, j.St, store.ImportedPass{Satellite: "NOAA 18", StartTS: now.Add(30 * time.Hour).Unix(), EndTS: now.Add(30*time.Hour + 600*time.Second).Unix(), MaxElevation: 50, State: store.StateScheduled})
	wd2.runOnce(context.Background(), now.Add(27*time.Hour))
	if len(n.checks()) != 4 {
		t.Errorf("healthy station must not alert: %v", n.checks())
	}
	if len(wd2.lastAlert) != 0 {
		t.Errorf("cleared checks still tracked: %v", wd2.lastAlert)
	}
}

func TestWatchdogAllPassesFailing(t *testing.T) {
	j, n, _ := newJobs(t)
	now := time.Now()
	for i := 1; i <= 3; i++ {
		insert(t, j.St, store.ImportedPass{Satellite: "NOAA 19", StartTS: now.Add(-time.Duration(i) * time.Hour).Unix(),
			EndTS: now.Add(-time.Duration(i)*time.Hour + 600*time.Second).Unix(), MaxElevation: 40, State: store.StateFailed, ErrorText: "x"})
	}
	insert(t, j.St, store.ImportedPass{Satellite: "NOAA 19", StartTS: now.Add(-40 * time.Hour).Unix(), EndTS: now.Add(-39 * time.Hour).Unix(), MaxElevation: 40, State: store.StateDecoded, FileBase: "y"})
	insert(t, j.St, store.ImportedPass{Satellite: "NOAA 18", StartTS: now.Add(2 * time.Hour).Unix(), EndTS: now.Add(3 * time.Hour).Unix(), MaxElevation: 40, State: store.StateScheduled})
	newWatchdog(j).runOnce(context.Background(), now)
	if got := n.checks(); len(got) != 1 || got[0] != "all_passes_failing" || !strings.Contains(n.alerts[0], "all 3 passes") {
		t.Errorf("alerts = %v", n.alerts)
	}
}

func TestWatchdogDisabled(t *testing.T) {
	j, n, cfg := newJobs(t)
	cfg.Watchdog.Enabled = false
	j.Prov.Set(cfg)
	newWatchdog(j).runOnce(context.Background(), time.Now())
	if len(n.alerts) != 0 {
		t.Error("disabled watchdog must not alert")
	}
}

func TestBestOfDayPicksStrongestAndAttachesArtifacts(t *testing.T) {
	j, n, cfg := newJobs(t)
	cfg.Daily.BestOfDayPush = true
	cfg.Daily.Mosaic.Enabled = true
	cfg.Daily.Mosaic.Suffixes = []string{"-221_projected.jpg"}
	j.Prov.Set(cfg)
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if now.Sub(day) < 2*time.Hour {
		now = day.Add(3 * time.Hour) // keep the fixture inside today
	}
	weak := insert(t, j.St, store.ImportedPass{Satellite: "NOAA 19", StartTS: day.Add(1 * time.Hour).Unix(), EndTS: day.Add(1*time.Hour + 600*time.Second).Unix(),
		MaxElevation: 80, State: store.StateDecoded, FileBase: "N", Daylight: ptr(true), MaxSNR: ptr(5.0)})
	strong := insert(t, j.St, store.ImportedPass{Satellite: "METEOR-M2 3", StartTS: day.Add(90 * time.Minute).Unix(), EndTS: day.Add(90*time.Minute + 600*time.Second).Unix(),
		MaxElevation: 45, State: store.StateDecoded, FileBase: "M", Daylight: ptr(true), MaxSNR: ptr(14.25)})
	j.St.AddImage(weak, "MSA", "N-MSA.jpg", "N-MSA.jpg")
	j.St.AddImage(strong, "polar-azel", "M-polar-azel.svg", "")
	j.St.AddImage(strong, "221_corrected", "M-221_corrected.jpg", "M-221_corrected.jpg")
	j.St.AddImage(strong, "MSA_corrected", "M-MSA_corrected.jpg", "M-MSA_corrected.jpg")
	for _, f := range []string{"N-MSA.jpg", "M-polar-azel.svg", "M-221_corrected.jpg", "M-MSA_corrected.jpg"} {
		touch(t, filepath.Join(cfg.Paths.Images, f))
	}
	mosaic := filepath.Join(cfg.Paths.Images, "mosaic-"+day.Format("20060102")+"-221_projected.jpg")
	touch(t, mosaic)

	j.bestOfDay(context.Background(), now)
	if len(n.summaries) != 1 {
		t.Fatalf("summaries = %v", n.summaries)
	}
	want := "Daily summary " + day.Format("2006-01-02") + " - Copenhagen | Best capture: METEOR-M2 3, max elev 45°, peak SNR 14.2 dB | 0 timelapse(s), 1 mosaic(s)"
	if n.summaries[0] != want {
		t.Errorf("annotation\n got %q\nwant %q", n.summaries[0], want)
	}
	if files := n.files[0]; len(files) != 2 || filepath.Base(files[0]) != "M-MSA_corrected.jpg" || files[1] != mosaic {
		t.Errorf("files = %v", files)
	}
}

func TestBestOfDayNothingToPush(t *testing.T) {
	j, n, cfg := newJobs(t)
	cfg.Daily.BestOfDayPush = true
	j.Prov.Set(cfg)
	j.bestOfDay(context.Background(), time.Now())
	if len(n.summaries) != 0 {
		t.Error("no captures: nothing must be pushed")
	}
}

func TestPruneAppliesRetention(t *testing.T) {
	j, _, cfg := newJobs(t)
	cfg.Retention.PruneImagesOlderThanDays = 10
	j.Prov.Set(cfg)
	now := time.Now()
	old := insert(t, j.St, store.ImportedPass{Satellite: "NOAA 19", StartTS: now.AddDate(0, 0, -11).Unix(), EndTS: now.AddDate(0, 0, -11).Unix() + 600,
		MaxElevation: 50, State: store.StateDecoded, FileBase: "OLD"})
	fresh := insert(t, j.St, store.ImportedPass{Satellite: "NOAA 19", StartTS: now.AddDate(0, 0, -2).Unix(), EndTS: now.AddDate(0, 0, -2).Unix() + 600,
		MaxElevation: 50, State: store.StateDecoded, FileBase: "NEW"})
	failedOld := insert(t, j.St, store.ImportedPass{Satellite: "NOAA 18", StartTS: now.AddDate(0, 0, -12).Unix(), EndTS: now.AddDate(0, 0, -12).Unix() + 600,
		MaxElevation: 50, State: store.StateFailed, ErrorText: "x"})
	for _, base := range []string{"OLD", "NEW"} {
		id := old
		if base == "NEW" {
			id = fresh
		}
		j.St.AddImage(id, "MCIR", base+"-MCIR.jpg", base+"-MCIR.jpg")
		j.St.AddImage(id, "website-thumbnail", "", base+"-website-thumbnail.jpg")
		touch(t, filepath.Join(cfg.Paths.Images, base+"-MCIR.jpg"))
		touch(t, filepath.Join(cfg.Paths.Thumbs, base+"-MCIR.jpg"))
		touch(t, filepath.Join(cfg.Paths.Thumbs, base+"-website-thumbnail.jpg"))
	}
	oldMosaic := filepath.Join(cfg.Paths.Images, "mosaic-"+now.AddDate(0, 0, -20).Format("20060102")+"-321_projected.jpg")
	newMosaic := filepath.Join(cfg.Paths.Images, "mosaic-"+now.Format("20060102")+"-321_projected.jpg")
	touch(t, oldMosaic)
	touch(t, newMosaic)
	staleWork := filepath.Join(cfg.Paths.Work, "pass-1")
	freshWork := filepath.Join(cfg.Paths.Work, "pass-2")
	touch(t, filepath.Join(staleWork, "satdump.log"))
	touch(t, filepath.Join(freshWork, "satdump.log"))
	os.Chtimes(staleWork, now.AddDate(0, 0, -8), now.AddDate(0, 0, -8))

	j.prune(now)

	for _, gone := range []string{filepath.Join(cfg.Paths.Images, "OLD-MCIR.jpg"), filepath.Join(cfg.Paths.Thumbs, "OLD-MCIR.jpg"),
		filepath.Join(cfg.Paths.Thumbs, "OLD-website-thumbnail.jpg"), oldMosaic, staleWork} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", gone)
		}
	}
	for _, kept := range []string{filepath.Join(cfg.Paths.Images, "NEW-MCIR.jpg"), newMosaic, freshWork} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept", kept)
		}
	}
	if p, _ := j.St.PassByID(old); p != nil {
		t.Error("old capture row still present")
	}
	if p, _ := j.St.PassByID(fresh); p == nil {
		t.Error("fresh capture row removed")
	}
	if p, _ := j.St.PassByID(failedOld); p == nil {
		t.Error("failed passes carry no files and stay for the stats/sky map")
	}

	// Retention off: nothing but stale work dirs is touched.
	cfg.Retention.PruneImagesOlderThanDays = 0
	j.Prov.Set(cfg)
	j.prune(now.AddDate(0, 0, 30))
	if p, _ := j.St.PassByID(fresh); p == nil {
		t.Error("retention disabled but capture pruned")
	}
}

func TestBestOfDayRebuildsArtifactsWithoutPushingWhenDisabled(t *testing.T) {
	j, n, cfg := newJobs(t)
	cfg.Daily.BestOfDayPush = false
	cfg.Daily.Mosaic.Enabled = true
	j.Prov.Set(cfg)
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	touch(t, filepath.Join(cfg.Paths.Images, "mosaic-"+day.Format("20060102")+"-221_projected.jpg"))
	j.bestOfDay(context.Background(), now)
	if len(n.summaries) != 0 {
		t.Errorf("best_of_day_push is off but a summary went out: %v", n.summaries)
	}
}

func TestRepresentativeImageSkipsStaleRows(t *testing.T) {
	j, _, cfg := newJobs(t)
	id := insert(t, j.St, store.ImportedPass{Satellite: "NOAA 19", StartTS: 1, EndTS: 2, MaxElevation: 1, State: store.StateDecoded, FileBase: "N", Daylight: ptr(true)})
	j.St.AddImage(id, "MSA", "N-MSA.jpg", "") // preferred, but not on disk
	j.St.AddImage(id, "HVC", "N-HVC.jpg", "")
	touch(t, filepath.Join(cfg.Paths.Images, "N-HVC.jpg"))
	p, _ := j.St.PassByID(id)
	if got := j.representativeImage(cfg, p); filepath.Base(got) != "N-HVC.jpg" {
		t.Errorf("picked %q, want the lower-priority image that exists", got)
	}
	os.Remove(filepath.Join(cfg.Paths.Images, "N-HVC.jpg"))
	if got := j.representativeImage(cfg, p); got != "" {
		t.Errorf("nothing on disk must yield no image, got %q", got)
	}
}
