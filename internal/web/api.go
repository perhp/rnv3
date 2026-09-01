package web

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
)

// Read-only JSON API plus an RSS feed, field-for-field what RN2 served:
//   /api/passes        upcoming passes
//   /api/captures      latest captures (?limit=N, max 100)
//   /api/capture?id=N  one capture including its image URLs
//   /api/status        current/next pass
//   /api/rss           RSS 2.0 of the latest captures

type apiPass struct {
	SatName      string  `json:"sat_name"`
	PassStart    int64   `json:"pass_start"`
	PassEnd      int64   `json:"pass_end"`
	MaxElev      int     `json:"max_elev"`
	StartAzimuth *int    `json:"pass_start_azimuth"`
	AzimuthAtMax *int    `json:"azimuth_at_max"`
	Direction    string  `json:"direction"`
	IsActive     int     `json:"is_active"`
	Status       *string `json:"status"`
}

type apiCapture struct {
	ID           int64    `json:"id"`
	SatName      string   `json:"sat_name"`
	PassStart    int64    `json:"pass_start"`
	PassEnd      int64    `json:"pass_end"`
	MaxElev      int      `json:"max_elev"`
	AzimuthAtMax *int     `json:"azimuth_at_max"`
	Direction    string   `json:"direction"`
	DaylightPass int      `json:"daylight_pass"`
	SatType      int      `json:"sat_type"` // RN2: 0 = Meteor, 1 = NOAA
	FilePath     string   `json:"file_path,omitempty"`
	Gain         *float64 `json:"gain"`
	MaxSNR       *float64 `json:"max_snr"`
	AvgSNR       *float64 `json:"avg_snr"`
	PageURL      string   `json:"page_url"`
	Images       []string `json:"images,omitempty"`
}

// satType maps a satellite name to RN2's numeric sat_type via the config,
// falling back to the name for imported history of satellites no longer
// configured.
func satType(cfg *config.Config, name string) int {
	for _, sat := range cfg.Satellites {
		if sat.Name == name {
			if sat.Type == config.SatNOAAAPT {
				return 1
			}
			return 0
		}
	}
	if strings.HasPrefix(strings.ToUpper(name), "NOAA") {
		return 1
	}
	return 0
}

func (s *Server) toAPICapture(r *http.Request, cfg *config.Config, p store.SchedulePass) apiCapture {
	daylight := 0
	if p.Daylight {
		daylight = 1
	}
	return apiCapture{
		ID: p.ID, SatName: p.Satellite, PassStart: p.StartTS, PassEnd: p.EndTS,
		MaxElev: roundInt(p.MaxElevation), AzimuthAtMax: roundPtr(p.AzimuthAtMax),
		Direction: dirLabel(p.Direction), DaylightPass: daylight, SatType: satType(cfg, p.Satellite),
		Gain: p.Gain, MaxSNR: p.MaxSNR, AvgSNR: p.AvgSNR,
		PageURL: fmt.Sprintf("%s/captures/listImages?pass_id=%d", baseURL(r), p.ID),
	}
}

func (s *Server) handleAPIPasses(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	passes, err := s.store.PassesEndingAfter(now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	out := make([]apiPass, 0, len(passes))
	for _, p := range passes {
		row := s.toSchedRow(p, "")
		out = append(out, apiPass{
			SatName: row.SatName, PassStart: row.PassStart, PassEnd: row.PassEnd, MaxElev: row.MaxElev,
			StartAzimuth: row.StartAzimuth, AzimuthAtMax: row.AzimuthAtMax, Direction: row.Direction,
			IsActive: row.IsActive, Status: row.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"server_time": now.Unix(), "passes": out})
}

func (s *Server) handleAPICaptures(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit")
	if limit == 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	cfg := s.prov.Get()
	captures, err := s.store.Captures(store.CaptureFilter{}, limit, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	out := make([]apiCapture, 0, len(captures))
	for _, p := range captures {
		out = append(out, s.toAPICapture(r, cfg, p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"server_time": time.Now().Unix(), "captures": out})
}

func (s *Server) handleAPICapture(w http.ResponseWriter, r *http.Request) {
	id := queryInt(r, "id")
	if id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing or invalid 'id' parameter"})
		return
	}
	p, err := s.store.PassByID(int64(id))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}
	if p == nil || !p.Decoded() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "capture not found"})
		return
	}
	cfg := s.prov.Get()
	out := s.toAPICapture(r, cfg, *p)
	out.FilePath = p.FileBase
	out.Images = []string{}
	for _, e := range s.enhancements(p.ID) {
		out.Images = append(out.Images, baseURL(r)+e.ImageURL)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	cur, _ := s.store.CurrentCapture(now)
	next, _ := s.store.NextPass(now)
	writeJSON(w, http.StatusOK, map[string]any{
		"server_time": now.Unix(),
		"current":     toStatusPass(cur),
		"next":        toStatusPass(next),
	})
}

// ---- RSS -------------------------------------------------------------------

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Items       []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string        `xml:"title"`
	Link        string        `xml:"link"`
	GUID        rssGUID       `xml:"guid"`
	PubDate     string        `xml:"pubDate"`
	Description string        `xml:"description"`
	Enclosure   *rssEnclosure `xml:"enclosure,omitempty"`
}

type rssGUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

func (s *Server) handleRSS(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get()
	base := baseURL(r)
	captures, err := s.store.Captures(store.CaptureFilter{}, 20, 0)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	feed := rssFeed{Version: "2.0", Channel: rssChannel{
		Title:       "Raspberry NOAA V3 Captures",
		Link:        base + "/captures",
		Description: "Latest weather satellite captures from this ground station",
		Items:       []rssItem{},
	}}
	for _, p := range captures {
		elev := roundInt(p.MaxElevation)
		start := time.Unix(p.StartTS, 0)
		link := fmt.Sprintf("%s/captures/listImages?pass_id=%d", base, p.ID)
		desc := fmt.Sprintf("%s pass at max elevation %d°", p.Satellite, elev)
		if p.MaxSNR != nil {
			desc += fmt.Sprintf(", peak SNR %s dB", f1(*p.MaxSNR))
		}
		item := rssItem{
			Title:       fmt.Sprintf("%s - %s (%d°)", p.Satellite, start.Format("2006-01-02 15:04"), elev),
			Link:        link,
			GUID:        rssGUID{IsPermaLink: "true", Value: link},
			PubDate:     start.Format(time.RFC1123Z),
			Description: desc,
		}
		// Same representative thumbnail as the gallery card.
		thumb := strings.TrimPrefix(galleryThumb(p), "/images/thumb/")
		if st, err := os.Stat(filepath.Join(cfg.Paths.Thumbs, thumb)); err == nil {
			item.Enclosure = &rssEnclosure{URL: base + thumbURL(thumb), Length: st.Size(), Type: "image/jpeg"}
		}
		feed.Channel.Items = append(feed.Channel.Items, item)
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=UTF-8")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(feed)
	w.Write([]byte("\n"))
}
