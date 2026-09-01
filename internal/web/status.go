package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/perhp/rnv3/internal/store"
)

// statusDoc is the passes page's live payload (RN2 /passes/status), served
// as JSON for polling and pushed over SSE.
type statusDoc struct {
	ServerTime int64          `json:"server_time"`
	Today      string         `json:"today"` // matches passes' date_key
	Current    *statusPass    `json:"current"`
	Next       *statusPass    `json:"next"`
	Passes     []schedRow     `json:"passes"`
	LogTail    []string       `json:"log_tail"`
	LogPassID  int64          `json:"log_pass_id"`
	LogSeq     uint64         `json:"log_seq"` // sequence of the last line in log_tail
	Latest     *latestDoc     `json:"latest"`
	Vitals     map[string]any `json:"vitals"`
	TLEAge     *int64         `json:"tle_age"`
}

type statusPass struct {
	SatName   string `json:"sat_name"`
	PassStart int64  `json:"pass_start"`
	PassEnd   int64  `json:"pass_end"`
	MaxElev   int    `json:"max_elev"`
	Status    string `json:"status,omitempty"`
}

type latestDoc struct {
	ID           int64    `json:"id"`
	SatName      string   `json:"sat_name"`
	PassStart    int64    `json:"pass_start"`
	MaxElev      int      `json:"max_elev"`
	Gain         *float64 `json:"gain"`
	MaxSNR       *float64 `json:"max_snr"`
	AvgSNR       *float64 `json:"avg_snr"`
	FrameLossPct *float64 `json:"frame_loss_pct"`
	TimeLabel    string   `json:"time_label"`
}

// schedRow is one schedule-table row, field names as RN2's JSON so the
// client renderer carries over.
type schedRow struct {
	ID              *int64   `json:"id"` // capture id when decoded
	SatName         string   `json:"sat_name"`
	IsActive        int      `json:"is_active"`
	PassStart       int64    `json:"pass_start"`
	PassEnd         int64    `json:"pass_end"`
	MaxElev         int      `json:"max_elev"`
	StartAzimuth    *int     `json:"pass_start_azimuth"`
	AzimuthAtMax    *int     `json:"azimuth_at_max"`
	Direction       string   `json:"direction"`
	Status          *string  `json:"status"`
	ErrorText       *string  `json:"error_text"`
	FramesReceived  *int     `json:"frames_received"`
	FramesExpected  *int     `json:"frames_expected"`
	FrameLossPct    *float64 `json:"frame_loss_pct"`
	LargestFrameGap *int     `json:"largest_frame_gap"`
	DateKey         string   `json:"date_key"`
	DateLabel       string   `json:"date_label"`
	StartLabel      string   `json:"start_label"`
	EndLabel        string   `json:"end_label"`
}

const dateKeyLayout = "01/02/06"

func toStatusPass(p *store.SchedulePass) *statusPass {
	if p == nil {
		return nil
	}
	sp := &statusPass{SatName: p.Satellite, PassStart: p.StartTS, PassEnd: p.EndTS, MaxElev: roundInt(p.MaxElevation)}
	if p.State == store.StateCapturing || p.State == store.StateProcessing {
		sp.Status = p.State
	}
	return sp
}

func (s *Server) toSchedRow(p store.SchedulePass, dateFormat string) schedRow {
	start := time.Unix(p.StartTS, 0)
	row := schedRow{
		SatName:         p.Satellite,
		PassStart:       p.StartTS,
		PassEnd:         p.EndTS,
		MaxElev:         roundInt(p.MaxElevation),
		StartAzimuth:    roundPtr(p.StartAzimuth),
		AzimuthAtMax:    roundPtr(p.AzimuthAtMax),
		Direction:       dirLabel(p.Direction),
		FramesReceived:  p.FramesReceived,
		FramesExpected:  p.FramesExpected,
		FrameLossPct:    p.FrameLossPct,
		LargestFrameGap: p.LargestFrameGap,
		DateKey:         start.Format(dateKeyLayout),
		DateLabel:       start.Format(dateFormat),
		StartLabel:      start.Format("15:04:05"),
		EndLabel:        time.Unix(p.EndTS, 0).Format("15:04:05"),
	}
	switch p.State {
	case store.StateScheduled, store.StateCapturing, store.StateProcessing:
		row.IsActive = 1
	}
	if p.State == store.StateDecoded {
		id := p.ID
		row.ID = &id
	} else if p.State != store.StateScheduled {
		st := p.State
		row.Status = &st
	}
	if p.ErrorText != "" {
		et := p.ErrorText
		row.ErrorText = &et
	}
	return row
}

func roundPtr(f *float64) *int {
	if f == nil {
		return nil
	}
	i := int(math.Round(*f))
	return &i
}

// buildStatus assembles the live payload. logLines > 0 includes that much
// of the decoder scrollback while a capture is running.
func (s *Server) buildStatus(now time.Time, logLines int) statusDoc {
	cfg := s.prov.Get()
	doc := statusDoc{
		ServerTime: now.Unix(),
		Today:      now.Format(dateKeyLayout),
		Passes:     []schedRow{},
		LogTail:    []string{},
		Vitals:     hostVitals(cfg.Paths.Images),
	}
	if cur, err := s.store.CurrentCapture(now); err == nil && cur != nil {
		doc.Current = toStatusPass(cur)
		if logLines > 0 {
			doc.LogTail, doc.LogPassID, doc.LogSeq = s.live.Snapshot(logLines)
		}
	}
	if next, err := s.store.NextPass(now); err == nil {
		doc.Next = toStatusPass(next)
	}
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if passes, err := s.store.PassesStartingAfter(todayStart); err == nil {
		for _, p := range passes {
			doc.Passes = append(doc.Passes, s.toSchedRow(p, cfg.Web.DateFormat))
		}
	}
	if latest, err := s.store.LatestCapture(); err == nil && latest != nil {
		doc.Latest = &latestDoc{
			ID: latest.ID, SatName: latest.Satellite, PassStart: latest.StartTS,
			MaxElev: roundInt(latest.MaxElevation), Gain: latest.Gain, MaxSNR: latest.MaxSNR,
			AvgSNR: latest.AvgSNR, FrameLossPct: latest.FrameLossPct,
			TimeLabel: time.Unix(latest.StartTS, 0).Format("15:04"),
		}
	}
	if age := s.tles.Age(); age >= 0 && age < 100*365*24*time.Hour {
		secs := int64(age.Seconds())
		doc.TLEAge = &secs
	}
	return doc
}

// handlePassesStatus is the polling endpoint (kept for parity and as the
// fallback when EventSource cannot connect).
func (s *Server) handlePassesStatus(w http.ResponseWriter, r *http.Request) {
	lines := queryInt(r, "lines")
	if lines == 0 {
		lines = 12
	}
	if lines > 500 {
		lines = 500
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.buildStatus(time.Now(), lines))
}

// statusInterval is how often the SSE stream re-sends the status document.
const statusInterval = 5 * time.Second

// logEvent is one streamed decoder line; the client drops any whose seq is
// covered by the snapshot it already holds, so a line published between the
// subscription and the snapshot is never shown twice.
type logEvent struct {
	Seq  uint64 `json:"seq"`
	Line string `json:"line"`
}

// handlePassesEvents streams the status document every statusInterval and
// each decoder output line as it is produced.
func (s *Server) handlePassesEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	lines, cancel := s.live.Subscribe()
	defer cancel()

	if !send("status", s.buildStatus(time.Now(), 500)) {
		return
	}
	ticker := time.NewTicker(statusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			if !send("log", logEvent{Seq: line.Seq, Line: line.Text}) {
				return
			}
		case <-ticker.C:
			if !send("status", s.buildStatus(time.Now(), 500)) {
				return
			}
		}
	}
}
