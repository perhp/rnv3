package web

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/process"
	"github.com/perhp/rnv3/internal/store"
)

// ---- /passes -----------------------------------------------------------------

type passesPage struct {
	SatvisBase string // "" when the instrument is disabled
	SatvisSrc  string
	Lat, Lon   float64
}

func (s *Server) handlePasses(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get()
	data := passesPage{Lat: cfg.Station.Latitude, Lon: cfg.Station.Longitude}
	if cfg.Web.Instruments.Satvis {
		gs := fmtNum(cfg.Station.Latitude) + "," + fmtNum(cfg.Station.Longitude)
		data.SatvisSrc = "https://satvis.space/?elements=Point,Label,SensorCone&layers=ArcGis&terrain=None&gs=" + gs + "&tags=Weather"
		data.SatvisBase = "https://satvis.space/?elements=Point,Label,SensorCone&layers=ArcGis&terrain=None&tags=Weather&gs=" + gs + "&sat="
	}
	s.render(w, r, "passes", "passes", data)
}

// ---- /captures ---------------------------------------------------------------

type pager struct {
	CurPage   int
	PageCount int
	PrevURL   string // "" when on the first page
	NextURL   string // "" when on the last page
}

func newPager(cur, count int, filterQS string) pager {
	p := pager{CurPage: cur, PageCount: count}
	if cur > 1 {
		p.PrevURL = fmt.Sprintf("?page_no=%d%s", cur-1, filterQS)
	}
	if cur < count {
		p.NextURL = fmt.Sprintf("?page_no=%d%s", cur+1, filterQS)
	}
	return p
}

// clampPage applies RN2's pagination sanity rules.
func clampPage(requested, total int) int {
	page := requested
	if page < 1 {
		page = 1
	}
	if page > total {
		page = total
	}
	if page < 1 {
		page = 1
	}
	return page
}

// pageCount is at least 1 so an empty gallery reads "Page 1 of 1".
func pageCount(items, perPage int) int {
	if items <= 0 {
		return 1
	}
	return int(math.Ceil(float64(items) / float64(perPage)))
}

type captureCard struct {
	ID           int64
	Satellite    string
	ThumbURL     string
	MaxElev      int
	EW           string
	StartAzimuth *int
	Direction    string
	StartLabel   string
	Gain         *float64
	MaxSNR       *float64
	AvgSNR       *float64
	Frames       *frameStats
}

type frameStats struct {
	Received, Expected, LargestGap int
	LossPct                        float64
}

func frames(p store.SchedulePass) *frameStats {
	if p.FramesReceived == nil {
		return nil
	}
	f := &frameStats{Received: *p.FramesReceived}
	if p.FramesExpected != nil {
		f.Expected = *p.FramesExpected
	}
	if p.FrameLossPct != nil {
		f.LossPct = *p.FrameLossPct
	}
	if p.LargestFrameGap != nil {
		f.LargestGap = *p.LargestFrameGap
	}
	return f
}

func (s *Server) toCard(p store.SchedulePass, dateTimeFormat string) captureCard {
	return captureCard{
		ID: p.ID, Satellite: p.Satellite, ThumbURL: galleryThumb(p),
		MaxElev: roundInt(p.MaxElevation), EW: ewLabel(p.AzimuthAtMax), StartAzimuth: roundPtr(p.StartAzimuth),
		Direction: dirLabel(p.Direction), StartLabel: time.Unix(p.StartTS, 0).Format(dateTimeFormat),
		Gain: p.Gain, MaxSNR: p.MaxSNR, AvgSNR: p.AvgSNR, Frames: frames(p),
	}
}

type capturesPage struct {
	Filter   store.CaptureFilter
	Sats     []string
	FilterQS string
	Pager    pager
	Captures []captureCard
	Elevs    []int
}

func parseFilter(r *http.Request) (store.CaptureFilter, string) {
	q := r.URL.Query()
	f := store.CaptureFilter{Satellite: q.Get("sat")}
	if dn := q.Get("daynight"); dn == "day" || dn == "night" {
		f.DayNight = dn
	}
	f.MinElevation = queryInt(r, "min_elev")
	var qs strings.Builder
	if f.Satellite != "" {
		qs.WriteString("&sat=" + url.QueryEscape(f.Satellite))
	}
	if f.DayNight != "" {
		qs.WriteString("&daynight=" + f.DayNight)
	}
	if f.MinElevation > 0 {
		fmt.Fprintf(&qs, "&min_elev=%d", f.MinElevation)
	}
	return f, qs.String()
}

func (s *Server) handleCaptures(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get()
	filter, filterQS := parseFilter(r)
	total, err := s.store.CountCaptures(filter)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	perPage := cfg.Web.CapturesPerPage
	pages := pageCount(total, perPage)
	page := clampPage(queryInt(r, "page_no"), pages)
	rows, err := s.store.Captures(filter, perPage, perPage*(page-1))
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	sats, _ := s.store.CaptureSatellites()
	data := capturesPage{Filter: filter, Sats: sats, FilterQS: filterQS, Pager: newPager(page, pages, filterQS),
		Captures: []captureCard{}, Elevs: []int{20, 40, 60, 80}}
	for _, p := range rows {
		data.Captures = append(data.Captures, s.toCard(p, cfg.Web.DateTimeFormat))
	}
	s.render(w, r, "captures", "captures", data)
}

// ---- /captures/listImages ----------------------------------------------------

// auxiliaryKinds are the non-imagery products, listed after the imagery in
// this fixed order (RN2 Capture::AUXILIARY_ENHANCEMENTS).
var auxiliaryKinds = []string{"spectrogram", "polar-azel", "polar-direction", "pristine", "histogram"}

// excludedKinds exist only to be served elsewhere.
var excludedKinds = map[string]bool{"website-thumbnail": true}

type enhancement struct {
	Kind     string
	ImageURL string
	ThumbURL string
}

// enhancements lists a capture's products: imagery alphabetically, then the
// auxiliary graphs in declared order. Products without a thumbnail (the
// SVG polar plots) use the image itself.
func (s *Server) enhancements(passID int64) []enhancement {
	images, err := s.store.ImagesForPass(passID)
	if err != nil {
		return nil
	}
	auxIndex := map[string]int{}
	for i, k := range auxiliaryKinds {
		auxIndex[k] = i
	}
	var imagery, aux []enhancement
	for _, im := range images {
		if excludedKinds[im.Kind] || im.Path == "" {
			continue
		}
		e := enhancement{Kind: im.Kind, ImageURL: imageURL(im.Path), ThumbURL: imageURL(im.Path)}
		if im.ThumbPath != "" {
			e.ThumbURL = thumbURL(im.ThumbPath)
		}
		if _, ok := auxIndex[im.Kind]; ok {
			aux = append(aux, e)
		} else {
			imagery = append(imagery, e)
		}
	}
	sort.Slice(imagery, func(i, j int) bool { return imagery[i].Kind < imagery[j].Kind })
	sort.Slice(aux, func(i, j int) bool { return auxIndex[aux[i].Kind] < auxIndex[aux[j].Kind] })
	return append(imagery, aux...)
}

type capturePage struct {
	Capture      captureCard
	Enhancements []enhancement
}

func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	id := queryInt(r, "pass_id")
	p, err := s.store.PassByID(int64(id))
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if id <= 0 || p == nil || !p.Decoded() {
		http.NotFound(w, r)
		return
	}
	cfg := s.prov.Get()
	s.render(w, r, "capture", "capture", capturePage{
		Capture:      s.toCard(*p, cfg.Web.DateTimeFormat),
		Enhancements: s.enhancements(p.ID),
	})
}

// ---- /stats ------------------------------------------------------------------

type recordDay struct {
	Index   int
	Title   string
	Nil     bool
	FailPct string // "" when zero
	OKPct   string
}

type recordTick struct {
	Label string // "" for an unlabelled slot
	Today bool
}

type satRow struct {
	Satellite    string
	Captures     int
	AvgElev      string
	AvgSNR       *float64
	SNRPct       int
	BestSNR      *float64
	AvgFrameLoss *float64
	LossKind     string // "", "warn", "bad"
	YieldPct     int
	LastCapture  string
}

type composite struct {
	Label     string
	File      string
	DateLabel string
}

type statsPage struct {
	Totals      store.Totals
	FirstLabel  string
	SuccessRate *int
	BestSNR     *float64
	Axis        int
	AxisMid     int
	ShowDatum   bool
	DatumPct    string
	Mean        string
	Record      []recordDay
	Ticks       []recordTick
	PerSat      []satRow
	Mosaics     []composite
	Timelapses  []composite
	SkyMap      bool
	SkyMapURL   string
	SkyMapFile  string
}

const recordDays = 30

// niceCeiling rounds the chart's top gridline up so the midpoint label is a
// whole number (RN2 Stat::niceCeiling).
func niceCeiling(peak int) int {
	switch {
	case peak <= 2:
		return 2
	case peak <= 4:
		return 4
	case peak <= 10:
		return peak + peak%2
	}
	step := 10
	if peak <= 40 {
		step = 5
	}
	return int(math.Ceil(float64(peak)/float64(step))) * step
}

func pct(part, whole float64) string {
	return fmt.Sprintf("%.1f", part/whole*100)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	cfg := s.prov.Get()
	now := time.Now()
	totals, err := s.store.StationTotals(now)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	record, _ := s.store.DailyRecord(recordDays, now)
	perSat, _ := s.store.PerSatellite()

	data := statsPage{Totals: totals, Record: []recordDay{}, Ticks: []recordTick{}, PerSat: []satRow{}}
	if totals.FirstCapture != nil {
		data.FirstLabel = time.Unix(*totals.FirstCapture, 0).Format(cfg.Web.DateFormat)
	}
	if totals.Attempted30d > 0 {
		rate := int(math.Round(100 * float64(totals.Attempted30d-totals.Failed30d) / float64(totals.Attempted30d)))
		data.SuccessRate = &rate
	}

	peak, decodedTotal := 0, 0
	for _, d := range record {
		if d.Decoded+d.Failed > peak {
			peak = d.Decoded + d.Failed
		}
		decodedTotal += d.Decoded
	}
	data.Axis = niceCeiling(peak)
	data.AxisMid = int(math.Round(float64(data.Axis) / 2))
	mean := float64(decodedTotal) / recordDays
	data.Mean = f1(mean)
	data.ShowDatum = mean/float64(data.Axis) >= 0.08
	data.DatumPct = pct(mean, float64(data.Axis))
	for i, d := range record {
		rd := recordDay{Index: i, Nil: d.Decoded+d.Failed == 0}
		rd.Title = fmt.Sprintf("%s · %d %s", d.Day.Format(cfg.Web.DateFormat), d.Decoded, L("decoded"))
		if d.Failed > 0 {
			rd.Title += fmt.Sprintf(", %d %s", d.Failed, L("failed"))
			rd.FailPct = pct(float64(d.Failed), float64(data.Axis))
		}
		if d.Decoded > 0 {
			rd.OKPct = pct(float64(d.Decoded), float64(data.Axis))
		}
		data.Record = append(data.Record, rd)
		last := i == len(record)-1
		tick := recordTick{Today: last}
		// A tick a week apart, plus today, dropping any weekly tick close
		// enough to the end that the two labels would collide.
		if last || (i%7 == 0 && len(record)-1-i > 3) {
			tick.Label = d.Day.Format("2 Jan")
		}
		data.Ticks = append(data.Ticks, tick)
	}

	best := 0.0
	for _, st := range perSat {
		if st.BestSNR != nil && *st.BestSNR > best {
			best = *st.BestSNR
		}
	}
	if best > 0 {
		b := best
		data.BestSNR = &b
	}
	for _, st := range perSat {
		row := satRow{Satellite: st.Satellite, Captures: st.Captures, AvgElev: f1(st.AvgElevation),
			AvgSNR: st.AvgSNR, BestSNR: st.BestSNR, AvgFrameLoss: st.AvgFrameLoss,
			LastCapture: time.Unix(st.LastCapture, 0).Format(cfg.Web.DateTimeFormat)}
		if st.AvgSNR != nil && best > 0 {
			row.SNRPct = int(math.Round(*st.AvgSNR / best * 100))
		}
		if st.AvgFrameLoss != nil {
			loss := *st.AvgFrameLoss
			row.YieldPct = int(math.Round(100 - loss))
			switch {
			case loss >= 20:
				row.LossKind = "bad"
			case loss >= 5:
				row.LossKind = "warn"
			}
		}
		data.PerSat = append(data.PerSat, row)
	}

	data.Mosaics = newestPerVariant(cfg, "mosaic-*.jpg", mosaicName)
	data.Timelapses = newestPerVariant(cfg, "timelapse-*.gif", timelapseName)
	if st, err := os.Stat(filepath.Join(cfg.Paths.Images, process.SkymapFilename)); err == nil {
		data.SkyMap = true
		data.SkyMapFile = process.SkymapFilename
		data.SkyMapURL = fmt.Sprintf("/images/%s?v=%d", process.SkymapFilename, st.ModTime().Unix())
	}
	s.render(w, r, "stats", "stats", data)
}

var (
	mosaicName    = regexp.MustCompile(`^mosaic-(\d{8})-(.+)\.jpg$`)
	timelapseName = regexp.MustCompile(`^timelapse-(\d{8})(?:-(.+))?\.gif$`)
)

// newestPerVariant maps projection variant → newest daily artifact. Names
// are <kind>-YYYYMMDD-<variant>.<ext>, so sorted names are chronological and
// the last seen per variant is the most recent. Old timelapses without a
// variant are labelled "all projections".
func newestPerVariant(cfg *config.Config, glob string, name *regexp.Regexp) []composite {
	files, _ := filepath.Glob(filepath.Join(cfg.Paths.Images, glob))
	sort.Strings(files)
	newest := map[string]composite{}
	for _, f := range files {
		m := name.FindStringSubmatch(filepath.Base(f))
		if m == nil {
			continue
		}
		variant := ""
		if len(m) > 2 {
			variant = m[2]
		}
		label := strings.ReplaceAll(variant, "_", " ")
		if label == "" {
			label = L("all_projections")
		}
		c := composite{Label: label, File: filepath.Base(f)}
		if day, err := time.ParseInLocation("20060102", m[1], time.Local); err == nil {
			c.DateLabel = day.Format(cfg.Web.DateFormat)
		}
		newest[variant] = c
	}
	keys := make([]string, 0, len(newest))
	for k := range newest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]composite, 0, len(keys))
	for _, k := range keys {
		out = append(out, newest[k])
	}
	return out
}
