// Package web serves the rnv3 panel: RN2's ops-console UI ported 1:1 onto
// html/template, with its URLs preserved so bookmarks and integrations keep
// working (/passes, /captures?page_no=N, /captures/listImages?pass_id=N,
// /stats, /admin/*, /api/*, /images/...).
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/livelog"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

//go:embed ui
var uiFS embed.FS

// Replanner wakes the scheduler after the plan changed from the admin page.
type Replanner interface{ Replan() }

// Server holds the panel's dependencies. Config is snapshotted per request.
type Server struct {
	prov     *config.Provider
	store    *store.Store
	tles     *tle.Manager
	live     *livelog.Hub
	replan   Replanner
	version  string
	started  time.Time
	pages    map[string]*template.Template
	sessions *sessionStore
}

// pageNames are the templates rendered inside base.html.
var pageNames = []string{"passes", "captures", "capture", "stats", "admin_passes", "admin_captures", "login"}

func New(prov *config.Provider, st *store.Store, tles *tle.Manager, live *livelog.Hub, replan Replanner, version string) (*Server, error) {
	if live == nil {
		live = livelog.New()
	}
	s := &Server{prov: prov, store: st, tles: tles, live: live, replan: replan, version: version,
		started: time.Now(), pages: map[string]*template.Template{}, sessions: newSessionStore()}
	funcs := template.FuncMap{
		"L":    L,
		"dt":   s.fmtDateTime,
		"d":    s.fmtDate,
		"t":    fmtTime,
		"f1":   f1,
		"num":  fmtNum,
		"gain": gainLabel,
		"add":  func(a, b int) int { return a + b },
	}
	for _, name := range pageNames {
		t, err := template.New("base.html").Funcs(funcs).ParseFS(uiFS,
			"ui/base.html", "ui/pagination.html", "ui/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/passes", http.StatusFound)
	})
	mux.HandleFunc("GET /passes", s.handlePasses)
	mux.HandleFunc("GET /passes/status", s.handlePassesStatus)
	mux.HandleFunc("GET /passes/events", s.handlePassesEvents)
	mux.HandleFunc("GET /captures", s.handleCaptures)
	mux.HandleFunc("GET /captures/listImages", s.handleCapture)
	mux.HandleFunc("GET /stats", s.handleStats)

	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/passes", http.StatusFound)
	})
	mux.HandleFunc("GET /admin/login", s.handleLoginForm)
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.Handle("POST /admin/logout", s.requireAdmin(s.handleLogout))
	mux.Handle("GET /admin/passes", s.requireAdmin(s.handleAdminPasses))
	mux.Handle("GET /admin/captures", s.requireAdmin(s.handleAdminCaptures))
	mux.Handle("POST /admin/deletePass", s.requireAdmin(s.handleDeletePass))
	mux.Handle("POST /admin/deleteCapture", s.requireAdmin(s.handleDeleteCapture))

	mux.HandleFunc("GET /api/passes", s.handleAPIPasses)
	mux.HandleFunc("GET /api/captures", s.handleAPICaptures)
	mux.HandleFunc("GET /api/capture", s.handleAPICapture)
	mux.HandleFunc("GET /api/status", s.handleAPIStatus)
	mux.HandleFunc("GET /api/rss", s.handleRSS)

	// Imagery straight from disk (nginx's /images alias in RN2). Thumbs get
	// their own mount so a thumbs dir outside the images tree still works.
	mux.Handle("GET /images/thumb/", http.StripPrefix("/images/thumb/", s.diskDir(func(c *config.Config) string { return c.Paths.Thumbs })))
	mux.Handle("GET /images/", http.StripPrefix("/images/", s.diskDir(func(c *config.Config) string { return c.Paths.Images })))

	assets, _ := fs.Sub(uiFS, "ui/assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /enhancement_details.html", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, uiFS, "ui/enhancement_details.html")
	})
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// diskDir serves files from a config-resolved directory, without directory
// listings, resolving the path per request so a SIGHUP path change applies.
func (s *Server) diskDir(dir func(*config.Config) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		http.FileServer(http.Dir(dir(s.prov.Get()))).ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.version,
		"uptime":  time.Since(s.started).Round(time.Second).String(),
	})
}

// pageData is what every template sees: the page-specific view model in
// Data plus the masthead/navigation context.
type pageData struct {
	Page      string // nav key: passes | captures | stats | admin
	Station   config.Station
	Inst      config.Instruments
	TZ        string
	TZOffset  string
	AdminAuth bool // admin pages are locked behind a login
	LoggedIn  bool
	CSRF      string
	Data      any
}

// render executes page inside base.html, buffering so a template error
// yields a clean 500 instead of a half page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page, nav string, data any) {
	cfg := s.prov.Get()
	now := time.Now()
	pd := pageData{
		Page:      nav,
		Station:   cfg.Station,
		Inst:      cfg.Web.Instruments,
		TZ:        tzName(now),
		TZOffset:  now.Format("-07:00"),
		AdminAuth: cfg.Web.Admin.Enabled,
		Data:      data,
	}
	if sess := s.sessions.get(r); sess != nil {
		pd.LoggedIn = sess.authed
		pd.CSRF = sess.csrf
	}
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "base.html", pd); err != nil {
		slog.Error("template render failed", "page", page, "err", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Write(buf.Bytes())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// baseURL reconstructs the externally visible origin for absolute links in
// the API and RSS (RN2: REQUEST_SCHEME://HTTP_HOST).
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---- template helpers -----------------------------------------------------

func (s *Server) fmtDateTime(ts int64) string {
	return time.Unix(ts, 0).Format(s.prov.Get().Web.DateTimeFormat)
}

func (s *Server) fmtDate(ts int64) string {
	return time.Unix(ts, 0).Format(s.prov.Get().Web.DateFormat)
}

func fmtTime(ts int64) string { return time.Unix(ts, 0).Format("15:04:05") }

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func roundInt(v float64) int { return int(math.Round(v)) }

// gainLabel renders the capture gain the way RN2 did: 0 = Auto, unknown =
// Unknown.
func gainLabel(g *float64) string {
	switch {
	case g == nil:
		return "Unknown"
	case *g == 0:
		return "Auto"
	default:
		return fmtNum(*g)
	}
}

// ewLabel is the E/W suffix after the max elevation: the side of the sky the
// pass culminates on.
func ewLabel(azimuthAtMax *float64) string {
	if azimuthAtMax == nil {
		return ""
	}
	if *azimuthAtMax >= 0 && *azimuthAtMax <= 180 {
		return "E"
	}
	return "W"
}

// dirLabel capitalises the stored direction (RN2 stored "Northbound").
func dirLabel(direction string) string {
	switch direction {
	case "northbound":
		return "Northbound"
	case "southbound":
		return "Southbound"
	}
	return direction
}

// satvisURL links a satellite name into satvis.space centred on the station.
func satvisURL(cfg *config.Config, satName string) string {
	return "https://satvis.space/?elements=Point,Label,Orbit%20track,Sensor%20cone&layers=OfflineHighres&tags=Weather&sat=" +
		strings.ReplaceAll(satName, " ", "%20") +
		"&gs=" + fmtNum(cfg.Station.Latitude) + "," + fmtNum(cfg.Station.Longitude)
}

// thumbURL / imageURL map stored (relative) file names onto the mounts.
func thumbURL(name string) string { return "/images/thumb/" + path.Clean("/" + name)[1:] }
func imageURL(name string) string { return "/images/" + path.Clean("/" + name)[1:] }

// galleryThumb is the card image for a capture: the registered website
// thumbnail when the pass has one, else RN2's NOAA rule (MSA by day, MCIR by
// night) for imported history.
func galleryThumb(p store.SchedulePass) string {
	if p.ThumbPath != "" {
		return thumbURL(p.ThumbPath)
	}
	if p.Daylight {
		return thumbURL(p.FileBase + "-MSA.jpg")
	}
	return thumbURL(p.FileBase + "-MCIR.jpg")
}

// queryInt parses a positive integer query parameter, 0 when absent/invalid.
func queryInt(r *http.Request, key string) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
