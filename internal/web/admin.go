package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/perhp/rnv3/internal/store"
)

// RN2 guarded /admin with HTTP Basic auth against a plaintext password and
// deleted via GET links. Here: a login form checked against a bcrypt hash,
// an HttpOnly session cookie, and POST + CSRF token for every mutation. When
// web.admin.enabled is false the pages are open (trusted LAN), but the
// session/CSRF machinery still runs so a stray GET can never delete.

const (
	sessionCookie = "rnv3_session"
	sessionTTL    = 12 * time.Hour
)

type session struct {
	id      string
	csrf    string
	authed  bool
	expires time.Time
}

type sessionStore struct {
	mu   sync.Mutex
	byID map[string]*session
}

func newSessionStore() *sessionStore { return &sessionStore{byID: map[string]*session{}} }

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// get returns the request's live session, or nil.
func (ss *sessionStore) get(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	sess, ok := ss.byID[c.Value]
	if !ok {
		return nil
	}
	if time.Now().After(sess.expires) {
		delete(ss.byID, c.Value)
		return nil
	}
	return sess
}

// create issues a new session and sets its cookie. The cookie is only
// marked Secure on TLS requests so a plain-HTTP LAN panel keeps working.
func (ss *sessionStore) create(w http.ResponseWriter, r *http.Request, authed bool) *session {
	sess := &session{id: randomToken(), csrf: randomToken(), authed: authed, expires: time.Now().Add(sessionTTL)}
	ss.mu.Lock()
	for id, old := range ss.byID { // opportunistic sweep
		if time.Now().After(old.expires) {
			delete(ss.byID, id)
		}
	}
	ss.byID[sess.id] = sess
	ss.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds()),
	})
	return sess
}

func (ss *sessionStore) destroy(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		ss.mu.Lock()
		delete(ss.byID, c.Value)
		ss.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

// requireAdmin gates the admin handlers: with auth enabled it demands an
// authenticated session (redirecting GETs to the login form); either way it
// guarantees a session exists (for the CSRF token) and checks the token on
// POST.
func (s *Server) requireAdmin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.prov.Get()
		sess := s.sessions.get(r)
		if cfg.Web.Admin.Enabled && (sess == nil || !sess.authed) {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			} else {
				http.Error(w, "login required", http.StatusForbidden)
			}
			return
		}
		if sess == nil {
			sess = s.sessions.create(w, r, false)
			// Make the new session visible to this request's render, which
			// looks it up from the request cookies.
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess.id})
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.csrf)) != 1 {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	})
}

// ---- login -------------------------------------------------------------------

type loginPage struct {
	Error string
	Next  string
}

// safeNext only allows redirects back into the admin area.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/admin") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/admin/passes"
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if !s.prov.Get().Web.Admin.Enabled {
		http.Redirect(w, r, "/admin/passes", http.StatusFound)
		return
	}
	if sess := s.sessions.get(r); sess != nil && sess.authed {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
		return
	}
	s.render(w, r, "login", "admin", loginPage{Next: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get()
	if !cfg.Web.Admin.Enabled {
		http.Redirect(w, r, "/admin/passes", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostForm.Get("next"))
	userOK := subtle.ConstantTimeCompare([]byte(r.PostForm.Get("username")), []byte(cfg.Web.Admin.Username)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(cfg.Web.Admin.PasswordHash), []byte(r.PostForm.Get("password"))) == nil
	if !userOK || !passOK {
		slog.Warn("admin login failed", "remote", r.RemoteAddr)
		time.Sleep(500 * time.Millisecond) // blunt brute-force damper
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login", "admin", loginPage{Error: L("login_failed"), Next: next})
		return
	}
	s.sessions.destroy(w, r) // rotate the id on privilege change
	s.sessions.create(w, r, true)
	slog.Info("admin login", "user", cfg.Web.Admin.Username, "remote", r.RemoteAddr)
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.destroy(w, r)
	http.Redirect(w, r, "/passes", http.StatusFound)
}

// ---- admin pages -------------------------------------------------------------

type adminPassRow struct {
	ID           int64
	Satellite    string
	SatvisURL    string // "" when the instrument is disabled
	DateSep      string // non-empty on the first row of a new day
	StartLabel   string
	EndLabel     string
	StartFull    string
	EndFull      string
	MaxElev      int
	EW           string
	ElevPct      int
	StartAzimuth *int
	Direction    string
	Inactive     bool
	Conflict     bool
}

type adminPassesPage struct {
	Status string // "success" | error text | ""
	Passes []adminPassRow
}

func (s *Server) handleAdminPasses(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get()
	now := time.Now()
	passes, err := s.store.PassesStartingAfter(now)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	data := adminPassesPage{Status: r.URL.Query().Get("status"), Passes: []adminPassRow{}}
	lastDate := ""
	var prevEnd int64
	for _, p := range passes {
		start, end := time.Unix(p.StartTS, 0), time.Unix(p.EndTS, 0)
		row := adminPassRow{
			ID: p.ID, Satellite: p.Satellite,
			StartLabel: start.Format("15:04:05"), EndLabel: end.Format("15:04:05"),
			StartFull: start.Format(cfg.Web.DateTimeFormat), EndFull: end.Format(cfg.Web.DateTimeFormat),
			MaxElev: roundInt(p.MaxElevation), EW: ewLabel(p.AzimuthAtMax), ElevPct: int(p.MaxElevation / 90 * 100),
			StartAzimuth: roundPtr(p.StartAzimuth), Direction: dirLabel(p.Direction),
			Inactive: p.State != store.StateScheduled, Conflict: prevEnd >= p.StartTS,
		}
		if cfg.Web.Instruments.Satvis {
			row.SatvisURL = satvisURL(cfg, p.Satellite)
		}
		if d := start.Format(cfg.Web.DateFormat); d != lastDate {
			row.DateSep = d
			lastDate = d
		}
		data.Passes = append(data.Passes, row)
		prevEnd = p.EndTS
	}
	s.render(w, r, "admin_passes", "admin", data)
}

func (s *Server) handleDeletePass(w http.ResponseWriter, r *http.Request) {
	id := formInt(r, "id")
	if id <= 0 {
		http.Redirect(w, r, "/admin/passes?status="+url.QueryEscape(L("fail_delete_missing_id")), http.StatusSeeOther)
		return
	}
	status := "success"
	if err := s.store.CancelPass(int64(id)); err != nil {
		slog.Error("cannot cancel pass", "pass_id", id, "err", err)
		status = "Could not delete pass: " + err.Error()
	} else {
		slog.Info("pass cancelled from admin page", "pass_id", id)
		if s.replan != nil {
			s.replan.Replan()
		}
	}
	http.Redirect(w, r, "/admin/passes?status="+url.QueryEscape(status), http.StatusSeeOther)
}

type adminCapturesPage struct {
	Status   string
	Pager    pager
	Captures []captureCard
}

func (s *Server) handleAdminCaptures(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get()
	total, err := s.store.CountCaptures(store.CaptureFilter{})
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	perPage := cfg.Web.AdminCapturesPerPage
	pages := pageCount(total, perPage)
	page := clampPage(queryInt(r, "page_no"), pages)
	rows, err := s.store.Captures(store.CaptureFilter{}, perPage, perPage*(page-1))
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	data := adminCapturesPage{Status: r.URL.Query().Get("status"), Pager: newPager(page, pages, ""), Captures: []captureCard{}}
	for _, p := range rows {
		data.Captures = append(data.Captures, s.toCard(p, cfg.Web.DateTimeFormat))
	}
	s.render(w, r, "admin_captures", "admin", data)
}

// handleDeleteCapture removes a capture's files (every registered image and
// thumbnail, including the website thumbnail) and its rows.
func (s *Server) handleDeleteCapture(w http.ResponseWriter, r *http.Request) {
	id := formInt(r, "id")
	back := "/admin/captures?page_no=" + url.QueryEscape(r.PostForm.Get("page_no"))
	if id <= 0 {
		http.Redirect(w, r, back+"&status="+url.QueryEscape(L("fail_delete_missing_id")), http.StatusSeeOther)
		return
	}
	cfg := s.prov.Get()
	status := "success"
	if err := s.deleteCapture(cfg.Paths.Images, cfg.Paths.Thumbs, int64(id)); err != nil {
		slog.Error("cannot delete capture", "pass_id", id, "err", err)
		status = "Could not delete capture: " + err.Error()
	} else {
		slog.Info("capture deleted from admin page", "pass_id", id)
	}
	http.Redirect(w, r, back+"&status="+url.QueryEscape(status), http.StatusSeeOther)
}

func (s *Server) deleteCapture(imagesDir, thumbsDir string, id int64) error {
	p, err := s.store.PassByID(id)
	if err != nil {
		return err
	}
	if p == nil {
		return os.ErrNotExist
	}
	images, err := s.store.ImagesForPass(id)
	if err != nil {
		return err
	}
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("cannot delete file", "path", path, "err", err)
		}
	}
	for _, im := range images {
		if im.Path != "" {
			remove(filepath.Join(imagesDir, filepath.Base(im.Path)))
		}
		if im.ThumbPath != "" {
			remove(filepath.Join(thumbsDir, filepath.Base(im.ThumbPath)))
		}
	}
	if p.FileBase != "" { // belt and braces for imported captures
		remove(filepath.Join(thumbsDir, p.FileBase+"-website-thumbnail.jpg"))
	}
	return s.store.DeletePass(id)
}

func formInt(r *http.Request, key string) int {
	n := 0
	for _, c := range r.PostForm.Get(key) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1<<31 {
			return 0
		}
	}
	return n
}
