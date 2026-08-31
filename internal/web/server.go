// Package web serves the rnv3 panel. M0 scope: a status page proving the
// daemon, config, and store are alive; the full ops-console UI port lands in M4.
package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	cfg     *config.Config
	store   *store.Store
	version string
	started time.Time
	tmpl    *template.Template
}

func New(cfg *config.Config, st *store.Store, version string) (*Server, error) {
	tmpl, err := template.ParseFS(uiFS, "ui/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, version: version, started: time.Now(), tmpl: tmpl}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleStatus)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
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
	Err        string
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	data := statusData{
		Version:    s.version,
		Uptime:     time.Since(s.started).Round(time.Second).String(),
		Station:    s.cfg.Station,
		SDRType:    s.cfg.SDR.Type,
		Satellites: s.cfg.Satellites,
	}
	if v, err := s.store.SchemaVersion(); err == nil {
		data.SchemaVer = v
	} else {
		data.Err = err.Error()
	}
	if p, i, err := s.store.Counts(); err == nil {
		data.PassCount, data.ImageCount = p, i
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
