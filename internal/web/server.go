// Package web serves the rnv3 panel. M1 scope: a status page with the live
// pass plan proving daemon/config/store/scheduler are alive; the full
// ops-console UI port lands in M4.
package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	prov    *config.Provider
	store   *store.Store
	tles    *tle.Manager
	version string
	started time.Time
	tmpl    *template.Template
}

func New(prov *config.Provider, st *store.Store, tles *tle.Manager, version string) (*Server, error) {
	tmpl, err := template.ParseFS(uiFS, "ui/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{prov: prov, store: st, tles: tles, version: version, started: time.Now(), tmpl: tmpl}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleStatus)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/passes", s.handleAPIPasses)
	return mux
}

type passRow struct {
	Satellite    string
	Start        string
	End          string
	MaxElevation int
	Direction    string
	State        string
	ErrorText    string
}

type statusData struct {
	Version    string
	Uptime     string
	Station    config.Station
	SDRType    string
	Satellites []config.Satellite
	SchemaVer  int
	PassCount  int
	ImageCount int
	TLEAge     string
	TLEStale   bool
	Upcoming   []passRow
	Err        string
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get() // per-request snapshot
	data := statusData{
		Version:    s.version,
		Uptime:     time.Since(s.started).Round(time.Second).String(),
		Station:    cfg.Station,
		SDRType:    cfg.SDR.Type,
		Satellites: cfg.Satellites,
	}
	if v, err := s.store.SchemaVersion(); err == nil {
		data.SchemaVer = v
	} else {
		data.Err = err.Error()
	}
	if p, i, err := s.store.Counts(); err == nil {
		data.PassCount, data.ImageCount = p, i
	}
	if age := s.tles.Age(); age < 100*365*24*time.Hour {
		data.TLEAge = age.Round(time.Minute).String()
		data.TLEStale = age > 48*time.Hour
	} else {
		data.TLEAge = "never fetched"
		data.TLEStale = true
	}
	if passes, err := s.store.UpcomingPasses(time.Now(), 12); err == nil {
		for _, p := range passes {
			data.Upcoming = append(data.Upcoming, passRow{
				Satellite:    p.Satellite,
				Start:        time.Unix(p.StartTS, 0).Format("Mon 15:04:05"),
				End:          time.Unix(p.EndTS, 0).Format("15:04:05"),
				MaxElevation: int(p.MaxElevation),
				Direction:    p.Direction,
				State:        p.State,
				ErrorText:    p.ErrorText,
			})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "status.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	type health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Uptime  string `json:"uptime"`
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health{
		Status:  "ok",
		Version: s.version,
		Uptime:  time.Since(s.started).Round(time.Second).String(),
	})
}

// handleAPIPasses is the seed of the public JSON API (full parity with RN2's
// /api/* endpoints lands in M4).
func (s *Server) handleAPIPasses(w http.ResponseWriter, r *http.Request) {
	type apiPass struct {
		Satellite    string  `json:"satellite"`
		PassStart    int64   `json:"pass_start"`
		PassEnd      int64   `json:"pass_end"`
		MaxElevation float64 `json:"max_elevation"`
		StartAzimuth float64 `json:"pass_start_azimuth"`
		AzimuthAtMax float64 `json:"azimuth_at_max"`
		Direction    string  `json:"direction"`
		State        string  `json:"state"`
		ErrorText    string  `json:"error_text,omitempty"`
	}
	passes, err := s.store.UpcomingPasses(time.Now(), 200)
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	out := struct {
		ServerTime int64     `json:"server_time"`
		Passes     []apiPass `json:"passes"`
	}{ServerTime: time.Now().Unix(), Passes: []apiPass{}}
	for _, p := range passes {
		out.Passes = append(out.Passes, apiPass{
			Satellite:    p.Satellite,
			PassStart:    p.StartTS,
			PassEnd:      p.EndTS,
			MaxElevation: p.MaxElevation,
			StartAzimuth: p.StartAzimuth,
			AzimuthAtMax: p.AzimuthAtMax,
			Direction:    p.Direction,
			State:        p.State,
			ErrorText:    p.ErrorText,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
