package publish

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

type delivery struct {
	Event    string
	Auth     string
	Station  string
	Envelope envelope
	File     []byte
	FileName string
	FileType string
}

type receiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []delivery
	fail  int // fail this many deliveries with 500
	token string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{token: "s3cret"}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail > 0 {
			r.fail--
			http.Error(w, "boom", 500)
			return
		}
		d := delivery{Event: req.Header.Get("X-Rnv3-Event"), Auth: req.Header.Get("Authorization"), Station: req.Header.Get("X-Rnv3-Station")}
		if req.Header.Get("X-Rnv3-Delivery") == "" || !strings.HasPrefix(req.Header.Get("User-Agent"), "rnv3/") {
			t.Errorf("missing delivery id / user agent: %v", req.Header)
		}
		ct := req.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "application/json"):
			json.NewDecoder(req.Body).Decode(&d.Envelope)
		case strings.HasPrefix(ct, "multipart/form-data"):
			if err := req.ParseMultipartForm(32 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			json.Unmarshal([]byte(req.FormValue("payload")), &d.Envelope)
			f, hdr, err := req.FormFile("file")
			if err == nil {
				d.File, _ = io.ReadAll(f)
				d.FileName, d.FileType = hdr.Filename, hdr.Header.Get("Content-Type")
			}
		}
		r.got = append(r.got, d)
		w.WriteHeader(204)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, d := range r.got {
		out = append(out, d.Event)
	}
	return out
}

func (r *receiver) last() delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[len(r.got)-1]
}

func ptr[T any](v T) *T { return &v }

func newPublisher(t *testing.T, rcv *receiver, ep config.PublishEndpoint) (*Publisher, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.Station.Name = "raspinoaa"
	cfg.Station.Location = "Copenhagen"
	cfg.Paths.Images = filepath.Join(dir, "images")
	os.MkdirAll(cfg.Paths.Images, 0o755)
	if ep.URL == "" {
		ep.URL = rcv.srv.URL
	}
	if ep.Name == "" {
		ep.Name = "site"
	}
	ep.Token = "s3cret"
	cfg.Publish.Endpoints = []config.PublishEndpoint{ep}
	p := New(config.NewProvider(cfg), st, "test")
	return p, cfg
}

func seedDecoded(t *testing.T, p *Publisher, cfg *config.Config, start int64) int64 {
	t.Helper()
	tx, _ := p.St.DB.Begin()
	id, _, err := p.St.InsertImported(tx, store.ImportedPass{Satellite: "METEOR-M2 3", StartTS: start, EndTS: start + 700, MaxElevation: 71.2,
		StartAzimuth: ptr(12.0), AzimuthAtMax: ptr(95.0), Direction: "northbound", State: store.StateDecoded, FileBase: "M",
		Daylight: ptr(true), Gain: ptr(40.2), MaxSNR: ptr(11.5), FramesReceived: ptr(900), FramesExpected: ptr(1000), FrameLossPct: ptr(10.0)})
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	p.St.AddImage(id, "221_projected", "M-221_projected.jpg", "M-221_projected.jpg")
	p.St.AddImage(id, "polar-azel", "M-polar-azel.svg", "")
	p.St.AddImage(id, "website-thumbnail", "", "M-website-thumbnail.jpg")
	os.WriteFile(filepath.Join(cfg.Paths.Images, "M-221_projected.jpg"), []byte("JPEGDATA"), 0o644)
	os.WriteFile(filepath.Join(cfg.Paths.Images, "M-polar-azel.svg"), []byte("<svg/>"), 0o644)
	return id
}

func TestPassDecodedDeliversMetadataThenImagesInOrder(t *testing.T) {
	rcv := newReceiver(t)
	p, cfg := newPublisher(t, rcv, config.PublishEndpoint{Images: true})
	id := seedDecoded(t, p, cfg, 1_800_000_000)

	p.PassDecoded(id)
	p.drain(context.Background())

	if got := rcv.events(); strings.Join(got, ",") != "pass.decoded,pass.image,pass.image" {
		t.Fatalf("events = %v", got)
	}
	first := rcv.got[0]
	if first.Auth != "Bearer s3cret" || first.Station != "raspinoaa" || first.Envelope.Version != 1 || first.Envelope.Station.Location != "Copenhagen" {
		t.Errorf("envelope = %+v", first)
	}
	raw, _ := json.Marshal(first.Envelope.Data)
	var data passDecodedData
	json.Unmarshal(raw, &data)
	if data.Pass.ID != id || data.Pass.Satellite != "METEOR-M2 3" || data.Pass.SatelliteType != "meteor-lrpt" || data.Pass.Status != "decoded" ||
		data.Pass.Start != "2027-01-15T08:00:00Z" || *data.Pass.MaxSNR != 11.5 || *data.Pass.FramesReceived != 900 || data.Pass.Direction != "northbound" {
		t.Errorf("pass data = %+v", data.Pass)
	}
	if len(data.Images) != 2 || data.Images[0].Name != "M-221_projected.jpg" || data.Images[0].Graph || data.Images[0].ContentType != "image/jpeg" ||
		data.Images[0].Size != 8 || !data.Images[1].Graph || data.Images[1].ContentType != "image/svg+xml" {
		t.Errorf("images = %+v", data.Images)
	}
	img := rcv.got[1]
	if string(img.File) != "JPEGDATA" || img.FileName != "M-221_projected.jpg" || img.FileType != "image/jpeg" {
		t.Errorf("image delivery = %+v", img)
	}
	var imgData passImageData
	raw, _ = json.Marshal(img.Envelope.Data)
	json.Unmarshal(raw, &imgData)
	if imgData.PassID != id || imgData.Image.Kind != "221_projected" {
		t.Errorf("image payload = %+v", imgData)
	}
	if n, _ := p.St.OutboxCount(""); n != 0 {
		t.Errorf("outbox not drained: %d", n)
	}
	// The website thumbnail is never sent; published state is recorded.
	for _, d := range rcv.got {
		if strings.Contains(d.FileName, "website-thumbnail") {
			t.Error("website thumbnail leaked")
		}
	}
	if ids, _ := p.St.UnpublishedDecoded("site", time.Unix(0, 0)); len(ids) != 0 {
		t.Errorf("pass still counts as unpublished: %v", ids)
	}
}

func TestFailureKeepsOrderAndBacksOff(t *testing.T) {
	rcv := newReceiver(t)
	p, cfg := newPublisher(t, rcv, config.PublishEndpoint{Images: true})
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return now }
	id := seedDecoded(t, p, cfg, 1_800_000_000)
	rcv.fail = 1
	p.PassDecoded(id)
	p.drain(context.Background())
	if len(rcv.events()) != 0 {
		t.Fatalf("a failed pass.decoded must block the endpoint's queue: %v", rcv.events())
	}
	// Another round before the backoff elapses sends nothing — the images
	// behind the deferred pass.decoded must not overtake it.
	p.drain(context.Background())
	if len(rcv.events()) != 0 {
		t.Fatalf("images overtook their deferred pass.decoded: %v", rcv.events())
	}
	head, _ := p.St.OutboxHead("site")
	if head == nil || head.Event != "pass.decoded" || head.Attempts != 1 || head.NextTS != now.Add(time.Minute).Unix() {
		t.Errorf("head after failure = %+v", head)
	}
	// Retry succeeds, and images follow in order.
	now = now.Add(2 * time.Minute)
	p.drain(context.Background())
	if got := rcv.events(); strings.Join(got, ",") != "pass.decoded,pass.image,pass.image" {
		t.Errorf("after retry: %v", got)
	}
	// Backoff caps at an hour; ancient entries are abandoned.
	e := store.OutboxEntry{Endpoint: "site", Event: EventPassDeleted, PassID: 1, Payload: "{}"}
	p.St.Enqueue(e)
	rcv.fail = 1
	p.drain(context.Background())
	if head, _ := p.St.OutboxHead("site"); head == nil || head.NextTS < now.Add(time.Minute).Unix() {
		t.Errorf("first retry sooner than a minute: %+v", head)
	}
	p.St.DB.Exec(`UPDATE outbox SET created_ts = ?`, now.Add(-8*24*time.Hour).Unix())
	p.drain(context.Background())
	if n, _ := p.St.OutboxCount(""); n != 0 {
		t.Errorf("week-old entry not abandoned: %d left", n)
	}
}

func TestEndpointFiltersAndRemovedEndpoint(t *testing.T) {
	rcv := newReceiver(t)
	p, cfg := newPublisher(t, rcv, config.PublishEndpoint{Images: false, Events: []string{"pass.decoded", "station.alert"}})
	id := seedDecoded(t, p, cfg, 1_800_000_000)
	p.PassDecoded(id)
	p.PassFailed(id)
	p.PassDeleted(id)
	p.drain(context.Background())
	if got := rcv.events(); strings.Join(got, ",") != "pass.decoded" {
		t.Errorf("filtered events = %v", got)
	}
	p.Alert(context.Background(), "disk_usage", "full")
	p.SendSchedule(context.Background())
	if got := rcv.events(); strings.Join(got, ",") != "pass.decoded,station.alert" {
		t.Errorf("direct events = %v", got)
	}
	// Endpoint removed from config: its queue is dropped.
	p.St.Enqueue(store.OutboxEntry{Endpoint: "site", Event: EventPassFailed, PassID: id})
	c2 := *cfg
	c2.Publish.Endpoints = nil
	p.Prov.Set(&c2)
	p.drain(context.Background())
	if n, _ := p.St.OutboxCount(""); n != 0 {
		t.Errorf("queue of removed endpoint kept: %d", n)
	}
}

func TestBackfillQueuesRecentUnpublishedOnly(t *testing.T) {
	rcv := newReceiver(t)
	p, cfg := newPublisher(t, rcv, config.PublishEndpoint{Images: true})
	now := time.Now()
	recent := seedDecoded(t, p, cfg, now.Add(-2*24*time.Hour).Unix())
	old := seedDecoded(t, p, cfg, now.Add(-60*24*time.Hour).Unix())
	done := seedDecoded(t, p, cfg, now.Add(-1*24*time.Hour).Unix())
	p.St.MarkPublished("site", done)
	p.Backfill()
	p.drain(context.Background())
	var ids []int64
	for _, d := range rcv.got {
		if d.Event == "pass.decoded" {
			raw, _ := json.Marshal(d.Envelope.Data)
			var data passDecodedData
			json.Unmarshal(raw, &data)
			ids = append(ids, data.Pass.ID)
		}
	}
	if len(ids) != 1 || ids[0] != recent {
		t.Errorf("backfilled %v, want only %d (old=%d done=%d)", ids, recent, old, done)
	}
	if got := rcv.events(); len(got) != 3 { // decoded + 2 images
		t.Errorf("events = %v", got)
	}
	// Running backfill again queues nothing.
	p.Backfill()
	if n, _ := p.St.OutboxCount(""); n != 0 {
		t.Errorf("backfill re-queued published passes: %d", n)
	}
}

func TestScheduleStatsAndDeleted(t *testing.T) {
	rcv := newReceiver(t)
	p, cfg := newPublisher(t, rcv, config.PublishEndpoint{Images: true})
	now := time.Now()
	tx, _ := p.St.DB.Begin()
	p.St.InsertImported(tx, store.ImportedPass{Satellite: "NOAA 19", StartTS: now.Unix() + 3600, EndTS: now.Unix() + 4200, MaxElevation: 62.4,
		StartAzimuth: ptr(190.0), AzimuthAtMax: ptr(250.0), Direction: "southbound", State: store.StateScheduled})
	p.St.InsertImported(tx, store.ImportedPass{Satellite: "NOAA 18", StartTS: now.Unix() + 5000, EndTS: now.Unix() + 5600, MaxElevation: 20, State: store.StateSkipped, ErrorText: "overlap"})
	tx.Commit()
	p.SendSchedule(context.Background())
	d := rcv.last()
	raw, _ := json.Marshal(d.Envelope.Data)
	var sched scheduleData
	json.Unmarshal(raw, &sched)
	if d.Event != "schedule.updated" || len(sched.Passes) != 1 || sched.Passes[0].Satellite != "NOAA 19" || *sched.Passes[0].AzimuthAtMax != 250 {
		t.Errorf("schedule = %+v", sched)
	}

	p.SendStats(context.Background())
	d = rcv.last()
	if d.Event != "station.stats" {
		t.Errorf("stats event = %s", d.Event)
	}
	raw, _ = json.Marshal(d.Envelope.Data)
	var stats map[string]any
	json.Unmarshal(raw, &stats)
	for _, k := range []string{"recorded_at", "cpu_temperature_c", "memory_used_bytes", "disk_total_bytes", "uptime_ms"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("stats lacks %s: %v", k, stats)
		}
	}

	id := seedDecoded(t, p, cfg, 1_800_000_000)
	p.St.MarkPublished("site", id)
	p.St.DeletePass(id)
	p.PassDeleted(id)
	p.drain(context.Background())
	d = rcv.last()
	raw, _ = json.Marshal(d.Envelope.Data)
	var del passDeletedData
	json.Unmarshal(raw, &del)
	if d.Event != "pass.deleted" || del.PassID != id {
		t.Errorf("deleted = %s %+v", d.Event, del)
	}
	if ids, _ := p.St.UnpublishedDecoded("site", time.Unix(0, 0)); len(ids) != 0 {
		t.Errorf("published state not forgotten: %v", ids)
	}

	lines := p.Test(context.Background())
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "ok") {
		t.Errorf("test = %v", lines)
	}
}

func TestUnauthorizedReceiverIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", 401)
	}))
	defer srv.Close()
	p, cfg := newPublisher(t, &receiver{srv: srv}, config.PublishEndpoint{})
	id := seedDecoded(t, p, cfg, 1_800_000_000)
	p.PassDecoded(id)
	p.drain(context.Background())
	due, _ := p.St.DueOutbox("site", time.Now().Add(2*time.Minute), 10)
	if len(due) == 0 || due[0].Attempts != 1 {
		t.Errorf("401 must defer the delivery: %+v", due)
	}
	if lines := p.Test(context.Background()); !strings.Contains(lines[0], "FAILED: HTTP 401") {
		t.Errorf("test = %v", lines)
	}
}
