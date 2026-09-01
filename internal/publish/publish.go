// Package publish pushes station events to HTTP receivers — the generic,
// backend-agnostic feed described in docs/webhooks.md. Pass events go
// through a durable outbox (retried with backoff, ordered per endpoint);
// schedule, stats and alerts are sent directly.
package publish

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/hostinfo"
	"github.com/perhp/rnv3/internal/store"
)

// Event names.
const (
	EventPassDecoded     = "pass.decoded"
	EventPassImage       = "pass.image"
	EventPassFailed      = "pass.failed"
	EventPassDeleted     = "pass.deleted"
	EventScheduleUpdated = "schedule.updated"
	EventStationStats    = "station.stats"
	EventStationAlert    = "station.alert"
)

const (
	envelopeVersion = 1
	requestTimeout  = 2 * time.Minute // large images on slow uplinks
	workerInterval  = 15 * time.Second
	statsInterval   = 5 * time.Minute
	batchSize       = 50
	minBackoff      = time.Minute
	maxBackoff      = time.Hour
	maxQueueAge     = 7 * 24 * time.Hour
)

// graphKinds are auxiliary plots rather than satellite imagery.
var graphKinds = map[string]bool{"polar-azel": true, "polar-direction": true, "spectrogram": true, "histogram": true}

// Publisher owns the outbox worker and the direct sends.
type Publisher struct {
	Prov    *config.Provider
	St      *store.Store
	Version string
	Client  *http.Client
	Log     *slog.Logger
	// Now and Sleep are seams for tests.
	Now func() time.Time

	kick chan struct{}
	mu   sync.Mutex
}

func New(prov *config.Provider, st *store.Store, version string) *Publisher {
	return &Publisher{Prov: prov, St: st, Version: version, Client: &http.Client{Timeout: requestTimeout},
		Log: slog.Default().With("component", "publish"), Now: time.Now, kick: make(chan struct{}, 1)}
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Enabled reports whether any endpoint is configured.
func (p *Publisher) Enabled() bool { return len(p.Prov.Get().Publish.Endpoints) > 0 }

// ---- envelope ----------------------------------------------------------------

type envelope struct {
	Version int         `json:"version"`
	Event   string      `json:"event"`
	SentAt  string      `json:"sent_at"`
	Station stationInfo `json:"station"`
	Data    any         `json:"data"`
}

type stationInfo struct {
	Name      string  `json:"name"`
	Location  string  `json:"location"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

func (p *Publisher) envelope(cfg *config.Config, event string, data any) envelope {
	return envelope{Version: envelopeVersion, Event: event, SentAt: p.now().UTC().Format(time.RFC3339),
		Station: stationInfo{Name: cfg.Station.Name, Location: cfg.Station.Location,
			Latitude: cfg.Station.Latitude, Longitude: cfg.Station.Longitude}, Data: data}
}

// PassData is the pass object of pass.* events.
type PassData struct {
	ID              int64    `json:"id"`
	Satellite       string   `json:"satellite"`
	SatelliteType   string   `json:"satellite_type"`
	Status          string   `json:"status"`
	Start           string   `json:"start"`
	End             string   `json:"end"`
	MaxElevation    float64  `json:"max_elevation"`
	StartAzimuth    *float64 `json:"start_azimuth"`
	AzimuthAtMax    *float64 `json:"azimuth_at_max"`
	Direction       string   `json:"direction"`
	Daylight        bool     `json:"daylight"`
	Gain            *float64 `json:"gain"`
	MaxSNR          *float64 `json:"max_snr"`
	AvgSNR          *float64 `json:"avg_snr"`
	FramesReceived  *int     `json:"frames_received"`
	FramesExpected  *int     `json:"frames_expected"`
	FrameLossPct    *float64 `json:"frame_loss_pct"`
	LargestFrameGap *int     `json:"largest_frame_gap"`
	Error           string   `json:"error,omitempty"`
}

// ImageData describes one image of a pass.
type ImageData struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Graph       bool   `json:"graph"`
}

type passDecodedData struct {
	Pass   PassData    `json:"pass"`
	Images []ImageData `json:"images"`
}

type passFailedData struct {
	Pass PassData `json:"pass"`
}

type passImageData struct {
	PassID int64     `json:"pass_id"`
	Image  ImageData `json:"image"`
}

type passDeletedData struct {
	PassID int64 `json:"pass_id"`
}

// SchedulePass is one row of schedule.updated.
type SchedulePass struct {
	Satellite    string   `json:"satellite"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	MaxElevation float64  `json:"max_elevation"`
	StartAzimuth *float64 `json:"start_azimuth"`
	AzimuthAtMax *float64 `json:"azimuth_at_max"`
	Direction    string   `json:"direction"`
}

type scheduleData struct {
	Passes []SchedulePass `json:"passes"`
}

type statsData struct {
	RecordedAt       string   `json:"recorded_at"`
	CPUTemperatureC  *float64 `json:"cpu_temperature_c"`
	CPUUsagePercent  *float64 `json:"cpu_usage_percent"`
	MemoryTotalBytes *uint64  `json:"memory_total_bytes"`
	MemoryUsedBytes  *uint64  `json:"memory_used_bytes"`
	DiskTotalBytes   *uint64  `json:"disk_total_bytes"`
	DiskUsedBytes    *uint64  `json:"disk_used_bytes"`
	UptimeMS         *int64   `json:"uptime_ms"`
	Load1m           *float64 `json:"load_1m"`
}

type alertData struct {
	Check   string `json:"check"`
	Message string `json:"message"`
}

func passData(cfg *config.Config, p *store.SchedulePass) PassData {
	satType := ""
	if sat, ok := cfg.SatelliteByName(p.Satellite); ok {
		satType = string(sat.Type)
	} else if strings.HasPrefix(strings.ToUpper(p.Satellite), "NOAA") {
		satType = string(config.SatNOAAAPT)
	} else {
		satType = string(config.SatMeteorLRPT)
	}
	return PassData{
		ID: p.ID, Satellite: p.Satellite, SatelliteType: satType, Status: p.State,
		Start: time.Unix(p.StartTS, 0).UTC().Format(time.RFC3339), End: time.Unix(p.EndTS, 0).UTC().Format(time.RFC3339),
		MaxElevation: p.MaxElevation, StartAzimuth: p.StartAzimuth, AzimuthAtMax: p.AzimuthAtMax, Direction: p.Direction,
		Daylight: p.Daylight, Gain: p.Gain, MaxSNR: p.MaxSNR, AvgSNR: p.AvgSNR, FramesReceived: p.FramesReceived,
		FramesExpected: p.FramesExpected, FrameLossPct: p.FrameLossPct, LargestFrameGap: p.LargestFrameGap,
		Error: p.ErrorText,
	}
}

// imagesOf lists a pass's publishable images (files on disk; the website
// thumbnail excluded).
func (p *Publisher) imagesOf(cfg *config.Config, passID int64) ([]ImageData, error) {
	rows, err := p.St.ImagesForPass(passID)
	if err != nil {
		return nil, err
	}
	var out []ImageData
	for _, im := range rows {
		if im.Path == "" || im.Kind == "website-thumbnail" {
			continue
		}
		st, err := os.Stat(filepath.Join(cfg.Paths.Images, im.Path))
		if err != nil {
			continue
		}
		out = append(out, ImageData{Name: im.Path, Kind: im.Kind, ContentType: contentType(im.Path), Size: st.Size(), Graph: graphKinds[im.Kind]})
	}
	return out, nil
}

func contentType(name string) string {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// ---- queueing ----------------------------------------------------------------

// endpointsFor lists configured endpoints subscribed to an event.
func endpointsFor(cfg *config.Config, event string) []config.PublishEndpoint {
	var out []config.PublishEndpoint
	for _, ep := range cfg.Publish.Endpoints {
		if ep.Wants(event) {
			out = append(out, ep)
		}
	}
	return out
}

// PassDecoded queues pass.decoded plus one pass.image per file for every
// subscribed endpoint. Implements capture.PassPublisher.
func (p *Publisher) PassDecoded(passID int64) {
	cfg := p.Prov.Get()
	images, err := p.imagesOf(cfg, passID)
	if err != nil {
		p.Log.Warn("cannot list images for publishing", "pass_id", passID, "err", err)
	}
	var entries []store.OutboxEntry
	for _, ep := range cfg.Publish.Endpoints {
		if ep.Wants(EventPassDecoded) {
			entries = append(entries, store.OutboxEntry{Endpoint: ep.Name, Event: EventPassDecoded, PassID: passID})
		}
		if ep.Wants(EventPassImage) {
			for _, im := range images {
				entries = append(entries, store.OutboxEntry{Endpoint: ep.Name, Event: EventPassImage, PassID: passID, ImageName: im.Name})
			}
		}
	}
	p.enqueue(entries)
}

// PassFailed queues pass.failed.
func (p *Publisher) PassFailed(passID int64) {
	var entries []store.OutboxEntry
	for _, ep := range endpointsFor(p.Prov.Get(), EventPassFailed) {
		entries = append(entries, store.OutboxEntry{Endpoint: ep.Name, Event: EventPassFailed, PassID: passID})
	}
	p.enqueue(entries)
}

// PassDeleted queues pass.deleted (the row is already gone, so the payload
// is rendered now) and forgets the pass's published state.
func (p *Publisher) PassDeleted(passID int64) {
	payload, _ := json.Marshal(passDeletedData{PassID: passID})
	var entries []store.OutboxEntry
	for _, ep := range endpointsFor(p.Prov.Get(), EventPassDeleted) {
		entries = append(entries, store.OutboxEntry{Endpoint: ep.Name, Event: EventPassDeleted, PassID: passID, Payload: string(payload)})
	}
	p.enqueue(entries)
	if err := p.St.ForgetPublished(passID); err != nil {
		p.Log.Warn("cannot forget published state", "pass_id", passID, "err", err)
	}
}

func (p *Publisher) enqueue(entries []store.OutboxEntry) {
	if len(entries) == 0 {
		return
	}
	if err := p.St.Enqueue(entries...); err != nil {
		p.Log.Error("cannot queue webhook deliveries", "err", err)
		return
	}
	p.Kick()
}

// Kick wakes the worker (after a queue change).
func (p *Publisher) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Backfill queues decoded passes newer than backfill_days that an endpoint
// has not received (startup, and when an endpoint is added).
func (p *Publisher) Backfill() {
	cfg := p.Prov.Get()
	if cfg.Publish.BackfillDays <= 0 {
		return
	}
	since := p.now().AddDate(0, 0, -cfg.Publish.BackfillDays)
	for _, ep := range cfg.Publish.Endpoints {
		if !ep.Wants(EventPassDecoded) {
			continue
		}
		ids, err := p.St.UnpublishedDecoded(ep.Name, since)
		if err != nil {
			p.Log.Warn("backfill query failed", "endpoint", ep.Name, "err", err)
			continue
		}
		var entries []store.OutboxEntry
		for _, id := range ids {
			entries = append(entries, store.OutboxEntry{Endpoint: ep.Name, Event: EventPassDecoded, PassID: id})
			if ep.Wants(EventPassImage) {
				images, _ := p.imagesOf(cfg, id)
				for _, im := range images {
					entries = append(entries, store.OutboxEntry{Endpoint: ep.Name, Event: EventPassImage, PassID: id, ImageName: im.Name})
				}
			}
		}
		if len(entries) > 0 {
			p.Log.Info("backfilling passes", "endpoint", ep.Name, "passes", len(ids), "deliveries", len(entries))
			p.enqueue(entries)
		}
	}
}

// ---- worker --------------------------------------------------------------------

// Run drives the outbox and the periodic station.stats until ctx ends.
// The two run independently: a long backfill of images must never delay
// the health samples (a receiver's "online" badge hangs off them).
func (p *Publisher) Run(ctx context.Context) {
	go p.statsLoop(ctx)
	p.Backfill()
	p.drain(ctx)
	ticker := time.NewTicker(workerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
			p.drain(ctx)
		case <-ticker.C:
			p.drain(ctx)
		}
	}
}

// statsLoop sends station.stats on start and every statsInterval.
func (p *Publisher) statsLoop(ctx context.Context) {
	if p.Enabled() {
		p.SendStats(ctx)
	}
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.SendStats(ctx)
		}
	}
}

// drain delivers every due entry, endpoint by endpoint, in queue order.
// One failure stops that endpoint for this round so order is preserved.
func (p *Publisher) drain(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg := p.Prov.Get()
	byName := map[string]config.PublishEndpoint{}
	for _, ep := range cfg.Publish.Endpoints {
		byName[ep.Name] = ep
	}
	if n, err := p.St.DeleteOutboxOlderThan(p.now().Add(-maxQueueAge)); err == nil && n > 0 {
		p.Log.Warn("abandoned undeliverable webhook events", "count", n)
	}
	names, err := p.St.OutboxEndpoints()
	if err != nil {
		p.Log.Error("cannot read outbox", "err", err)
		return
	}
	for _, name := range names {
		ep, ok := byName[name]
		if !ok {
			if n, _ := p.St.DeleteOutboxEndpoint(name); n > 0 {
				p.Log.Info("dropped queue of removed endpoint", "endpoint", name, "entries", n)
			}
			continue
		}
		p.drainEndpoint(ctx, cfg, ep)
	}
}

func (p *Publisher) drainEndpoint(ctx context.Context, cfg *config.Config, ep config.PublishEndpoint) {
	delivered := 0
	defer func() {
		if delivered > 0 {
			left, _ := p.St.OutboxCount(ep.Name)
			p.Log.Info("webhook deliveries sent", "endpoint", ep.Name, "sent", delivered, "queued", left)
		}
	}()
	for {
		// The queue is strictly ordered: while the oldest entry is waiting
		// out its backoff, nothing behind it may be sent (a pass's images
		// must never arrive before its pass.decoded).
		head, err := p.St.OutboxHead(ep.Name)
		if err != nil || head == nil || head.NextTS > p.now().Unix() {
			return
		}
		due, err := p.St.DueOutbox(ep.Name, p.now(), batchSize)
		if err != nil || len(due) == 0 {
			return
		}
		for _, e := range due {
			if ctx.Err() != nil {
				return
			}
			if err := p.deliver(ctx, cfg, ep, e); err != nil {
				attempts := e.Attempts + 1
				delay := minBackoff << uint(min(attempts-1, 10))
				if delay > maxBackoff {
					delay = maxBackoff
				}
				p.Log.Warn("webhook delivery failed", "endpoint", ep.Name, "event", e.Event, "pass_id", e.PassID,
					"attempt", attempts, "retry_in", delay, "err", err)
				p.St.DeferOutbox(e.ID, attempts, p.now().Add(delay))
				return // keep order: nothing later for this endpoint until this one succeeds
			}
			p.St.DeleteOutbox(e.ID)
			delivered++
		}
		if len(due) < batchSize {
			return
		}
	}
}

// errSkip marks an entry whose source is gone; it is dropped, not retried.
var errSkip = errors.New("skip")

// deliver sends one outbox entry.
func (p *Publisher) deliver(ctx context.Context, cfg *config.Config, ep config.PublishEndpoint, e store.OutboxEntry) error {
	switch e.Event {
	case EventPassDecoded, EventPassFailed:
		pass, err := p.St.PassByID(e.PassID)
		if err != nil {
			return err
		}
		if pass == nil {
			p.Log.Info("pass gone before delivery, skipping", "pass_id", e.PassID, "event", e.Event)
			return nil
		}
		var data any
		if e.Event == EventPassDecoded {
			images, _ := p.imagesOf(cfg, e.PassID)
			if images == nil {
				images = []ImageData{}
			}
			data = passDecodedData{Pass: passData(cfg, pass), Images: images}
		} else {
			data = passFailedData{Pass: passData(cfg, pass)}
		}
		if err := p.postJSON(ctx, ep, e.Event, p.envelope(cfg, e.Event, data)); err != nil {
			return err
		}
		if e.Event == EventPassDecoded {
			p.St.MarkPublished(ep.Name, e.PassID)
		}
		return nil
	case EventPassImage:
		path := filepath.Join(cfg.Paths.Images, filepath.Base(e.ImageName))
		st, err := os.Stat(path)
		if err != nil {
			p.Log.Info("image gone before delivery, skipping", "pass_id", e.PassID, "image", e.ImageName)
			return nil
		}
		kind := ""
		if rows, err := p.St.ImagesForPass(e.PassID); err == nil {
			for _, im := range rows {
				if im.Path == e.ImageName {
					kind = im.Kind
				}
			}
		}
		img := ImageData{Name: e.ImageName, Kind: kind, ContentType: contentType(e.ImageName), Size: st.Size(), Graph: graphKinds[kind]}
		return p.postFile(ctx, ep, p.envelope(cfg, EventPassImage, passImageData{PassID: e.PassID, Image: img}), path, img)
	case EventPassDeleted:
		var data passDeletedData
		json.Unmarshal([]byte(e.Payload), &data)
		return p.postJSON(ctx, ep, EventPassDeleted, p.envelope(cfg, EventPassDeleted, data))
	}
	p.Log.Warn("unknown outbox event, dropping", "event", e.Event)
	return nil
}

// ---- direct sends ------------------------------------------------------------

// SendSchedule pushes schedule.updated with the current plan (scheduled
// passes only) to every subscribed endpoint.
func (p *Publisher) SendSchedule(ctx context.Context) {
	cfg := p.Prov.Get()
	eps := endpointsFor(cfg, EventScheduleUpdated)
	if len(eps) == 0 {
		return
	}
	passes, err := p.St.PassesEndingAfter(p.now())
	if err != nil {
		p.Log.Warn("cannot read schedule for publishing", "err", err)
		return
	}
	data := scheduleData{Passes: []SchedulePass{}}
	for _, ps := range passes {
		if ps.State != store.StateScheduled {
			continue
		}
		data.Passes = append(data.Passes, SchedulePass{
			Satellite: ps.Satellite, Start: time.Unix(ps.StartTS, 0).UTC().Format(time.RFC3339),
			End: time.Unix(ps.EndTS, 0).UTC().Format(time.RFC3339), MaxElevation: ps.MaxElevation,
			StartAzimuth: ps.StartAzimuth, AzimuthAtMax: ps.AzimuthAtMax, Direction: ps.Direction,
		})
	}
	env := p.envelope(cfg, EventScheduleUpdated, data)
	for _, ep := range eps {
		if err := p.postJSON(ctx, ep, EventScheduleUpdated, env); err != nil {
			p.Log.Warn("schedule.updated delivery failed", "endpoint", ep.Name, "err", err)
		}
	}
}

// SendStats pushes one station.stats sample.
func (p *Publisher) SendStats(ctx context.Context) {
	cfg := p.Prov.Get()
	eps := endpointsFor(cfg, EventStationStats)
	if len(eps) == 0 {
		return
	}
	s := hostinfo.Read(cfg.Paths.Images, 0)
	data := statsData{RecordedAt: s.RecordedAt.UTC().Format(time.RFC3339), CPUTemperatureC: s.CPUTemperatureC,
		CPUUsagePercent: s.CPUUsagePercent, MemoryTotalBytes: s.MemoryTotalBytes, MemoryUsedBytes: s.MemoryUsedBytes,
		DiskTotalBytes: s.DiskTotalBytes, DiskUsedBytes: s.DiskUsedBytes, UptimeMS: s.UptimeMS, Load1m: s.Load1m}
	env := p.envelope(cfg, EventStationStats, data)
	for _, ep := range eps {
		if err := p.postJSON(ctx, ep, EventStationStats, env); err != nil {
			p.Log.Warn("station.stats delivery failed", "endpoint", ep.Name, "err", err)
		}
	}
}

// Alert pushes station.alert (watchdog).
func (p *Publisher) Alert(ctx context.Context, check, message string) {
	cfg := p.Prov.Get()
	env := p.envelope(cfg, EventStationAlert, alertData{Check: check, Message: message})
	for _, ep := range endpointsFor(cfg, EventStationAlert) {
		if err := p.postJSON(ctx, ep, EventStationAlert, env); err != nil {
			p.Log.Warn("station.alert delivery failed", "endpoint", ep.Name, "err", err)
		}
	}
}

// Test sends a synthetic station.stats to every endpoint and returns one
// line per endpoint (rnv3 -publish-test).
func (p *Publisher) Test(ctx context.Context) []string {
	cfg := p.Prov.Get()
	if len(cfg.Publish.Endpoints) == 0 {
		return []string{"no publish endpoints configured"}
	}
	s := hostinfo.Read(cfg.Paths.Images, 0)
	env := p.envelope(cfg, EventStationStats, statsData{RecordedAt: s.RecordedAt.UTC().Format(time.RFC3339),
		CPUTemperatureC: s.CPUTemperatureC, CPUUsagePercent: s.CPUUsagePercent, MemoryTotalBytes: s.MemoryTotalBytes,
		MemoryUsedBytes: s.MemoryUsedBytes, DiskTotalBytes: s.DiskTotalBytes, DiskUsedBytes: s.DiskUsedBytes,
		UptimeMS: s.UptimeMS, Load1m: s.Load1m})
	var out []string
	for _, ep := range cfg.Publish.Endpoints {
		if err := p.postJSON(ctx, ep, EventStationStats, env); err != nil {
			out = append(out, fmt.Sprintf("%s (%s): FAILED: %v", ep.Name, ep.URL, err))
		} else {
			out = append(out, fmt.Sprintf("%s (%s): ok", ep.Name, ep.URL))
		}
	}
	return out
}

// ---- transport -----------------------------------------------------------------

func (p *Publisher) postJSON(ctx context.Context, ep config.PublishEndpoint, event string, env envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	p.headers(req, ep, event, env.Station.Name)
	return p.send(req)
}

// postFile streams a multipart body: the JSON envelope in "payload" and
// the image bytes in "file".
func (p *Publisher) postFile(ctx context.Context, ep config.PublishEndpoint, env envelope, path string, img ImageData) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var err error
		defer func() { pw.CloseWithError(err) }()
		var part io.Writer
		hdr := mimeHeader(map[string]string{"Content-Disposition": `form-data; name="payload"`, "Content-Type": "application/json"})
		if part, err = mw.CreatePart(hdr); err != nil {
			return
		}
		if _, err = part.Write(payload); err != nil {
			return
		}
		hdr = mimeHeader(map[string]string{
			"Content-Disposition": fmt.Sprintf(`form-data; name="file"; filename="%s"`, filepath.Base(path)),
			"Content-Type":        img.ContentType,
		})
		if part, err = mw.CreatePart(hdr); err != nil {
			return
		}
		if _, err = io.Copy(part, f); err != nil {
			return
		}
		err = mw.Close()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	p.headers(req, ep, EventPassImage, env.Station.Name)
	return p.send(req)
}

func (p *Publisher) headers(req *http.Request, ep config.PublishEndpoint, event, station string) {
	if ep.Token != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Token)
	}
	req.Header.Set("X-Rnv3-Event", event)
	req.Header.Set("X-Rnv3-Delivery", deliveryID())
	req.Header.Set("X-Rnv3-Station", station)
	req.Header.Set("User-Agent", "rnv3/"+p.Version)
}

func (p *Publisher) send(req *http.Request) error {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func deliveryID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // UUID v4 shape
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
