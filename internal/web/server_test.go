package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/livelog"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
)

type fakeReplanner struct{ calls int }

func (f *fakeReplanner) Replan() { f.calls++ }

type fixture struct {
	srv    *httptest.Server
	store  *store.Store
	prov   *config.Provider
	live   *livelog.Hub
	replan *fakeReplanner
	images string
	thumbs string
	now    time.Time
	// ids of seeded passes
	decoded, failed, scheduled, skipped, cancelled int64
}

func ptr[T any](v T) *T { return &v }

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.Paths.DataDir = dir
	cfg.Paths.Images = filepath.Join(dir, "images")
	cfg.Paths.Thumbs = filepath.Join(dir, "images", "thumb")
	cfg.Web.Admin.Enabled = false
	cfg.Web.CapturesPerPage = 2
	os.MkdirAll(cfg.Paths.Thumbs, 0o755)

	f := &fixture{store: st, prov: config.NewProvider(cfg), live: livelog.New(), replan: &fakeReplanner{},
		images: cfg.Paths.Images, thumbs: cfg.Paths.Thumbs, now: time.Now()}
	f.seed(t)

	s, err := New(f.prov, st, tle.NewManager(dir), f.live, f.replan, "test")
	if err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) seed(t *testing.T) {
	t.Helper()
	now := f.now.Unix()
	tx, err := f.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert := func(p store.ImportedPass) int64 {
		id, ok, err := f.store.InsertImported(tx, p)
		if err != nil || !ok {
			t.Fatalf("seed insert: ok=%v err=%v", ok, err)
		}
		return id
	}
	// Past passes are pinned just after local midnight so they always fall
	// within "today" for the status document, whatever the wall clock says.
	today := time.Date(f.now.Year(), f.now.Month(), f.now.Day(), 0, 0, 0, 0, f.now.Location()).Unix()
	f.decoded = insert(store.ImportedPass{
		Satellite: "NOAA 19", StartTS: today + 40, EndTS: today + 50, MaxElevation: 62.4,
		StartAzimuth: ptr(190.0), AzimuthAtMax: ptr(250.0), Direction: "northbound", State: store.StateDecoded,
		FileBase: "NOAA-19-20260901-120000", Daylight: ptr(true), Gain: ptr(29.7), MaxSNR: ptr(12.34), AvgSNR: ptr(8.9),
	})
	f.failed = insert(store.ImportedPass{
		Satellite: "METEOR-M2 3", StartTS: today + 10, EndTS: today + 20, MaxElevation: 40,
		AzimuthAtMax: ptr(90.0), Direction: "southbound", State: store.StateFailed, ErrorText: "decoder produced no images",
		FramesReceived: ptr(100), FramesExpected: ptr(400), FrameLossPct: ptr(75.0), LargestFrameGap: ptr(120),
	})
	f.scheduled = insert(store.ImportedPass{
		Satellite: "METEOR-M2 4", StartTS: now + 1800, EndTS: now + 2400, MaxElevation: 71,
		StartAzimuth: ptr(20.0), AzimuthAtMax: ptr(100.0), Direction: "northbound", State: store.StateScheduled,
	})
	f.skipped = insert(store.ImportedPass{
		Satellite: "NOAA 18", StartTS: now + 2000, EndTS: now + 2600, MaxElevation: 35,
		State: store.StateSkipped, ErrorText: "overlaps METEOR-M2 4",
	})
	f.cancelled = insert(store.ImportedPass{
		Satellite: "NOAA 15", StartTS: now + 5000, EndTS: now + 5600, MaxElevation: 50, State: store.StateCancelled,
	})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	base := "NOAA-19-20260901-120000"
	for _, kind := range []string{"MCIR", "MSA"} {
		f.store.AddImage(f.decoded, kind, base+"-"+kind+".jpg", base+"-"+kind+".jpg")
		touch(t, filepath.Join(f.images, base+"-"+kind+".jpg"))
		touch(t, filepath.Join(f.thumbs, base+"-"+kind+".jpg"))
	}
	f.store.AddImage(f.decoded, "polar-azel", base+"-polar-azel.svg", "")
	touch(t, filepath.Join(f.images, base+"-polar-azel.svg"))
	f.store.AddImage(f.decoded, "website-thumbnail", "", base+"-website-thumbnail.jpg")
	touch(t, filepath.Join(f.thumbs, base+"-website-thumbnail.jpg"))
}

func (f *fixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	res, err := http.Get(f.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("response is missing %q", w)
		}
	}
}

func TestPagesRender(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		path  string
		wants []string
	}{
		{"/passes", []string{"live-term", "Pass schedule", "/passes/events", "satvis.space"}},
		{"/captures?page_no=1", []string{"NOAA 19", "NOAA-19-20260901-120000-website-thumbnail.jpg", "62&#176;W", "12.3 dB", "Page 1 of 1"}},
		{"/captures/listImages?pass_id=" + itoa(f.decoded), []string{"MCIR", "MSA", "polar-azel", "/images/NOAA-19-20260901-120000-polar-azel.svg", "Enhancement details", "8.9 dB"}},
		{"/stats", []string{"Station record", "NOAA 19", "12.3<span class=\"unit\">dB</span>", "Horizon survey"}},
		{"/admin/passes", []string{"METEOR-M2 4", "NOAA 18", "confirm-form", "csrf"}},
		{"/admin/captures", []string{"NOAA 19", "deleteCapture"}},
		{"/enhancement_details.html", []string{"Enhancement reference"}},
		{"/assets/css/rn2.css", []string{".masthead"}},
		{"/assets/js/rn2.js", []string{"confirm-delete"}},
	}
	for _, c := range cases {
		status, body := f.get(t, c.path)
		if status != 200 {
			t.Errorf("%s: status %d\n%s", c.path, status, body)
			continue
		}
		mustContain(t, body, c.wants...)
		if strings.Contains(body, "NOAA 15") {
			t.Errorf("%s: cancelled pass must be hidden", c.path)
		}
	}
	// The website thumbnail never shows as an enhancement.
	_, body := f.get(t, "/captures/listImages?pass_id="+itoa(f.decoded))
	if strings.Contains(body, "website-thumbnail") {
		t.Error("website thumbnail listed as an enhancement")
	}
	if status, _ := f.get(t, "/captures/listImages?pass_id="+itoa(f.failed)); status != 404 {
		t.Errorf("failed pass has no capture page, got %d", status)
	}
	if status, _ := f.get(t, "/images/"); status != 404 {
		t.Errorf("directory listing must be refused, got %d", status)
	}
	if status, _ := f.get(t, "/images/thumb/NOAA-19-20260901-120000-MCIR.jpg"); status != 200 {
		t.Errorf("thumb not served, got %d", status)
	}
}

func TestGalleryFiltersAndPagination(t *testing.T) {
	f := newFixture(t)
	// Add two more captures so the 2-per-page gallery paginates.
	tx, _ := f.store.DB.Begin()
	for i := 1; i <= 2; i++ {
		f.store.InsertImported(tx, store.ImportedPass{Satellite: "METEOR-M2 3", StartTS: f.now.Unix() - int64(10000*i), EndTS: f.now.Unix() - int64(10000*i) + 600,
			MaxElevation: 25, State: store.StateDecoded, FileBase: "M", Daylight: ptr(false)})
	}
	tx.Commit()

	_, body := f.get(t, "/captures?page_no=1")
	mustContain(t, body, "Page 1 of 2", "?page_no=2")
	_, body = f.get(t, "/captures?page_no=99") // clamped to the last page
	mustContain(t, body, "Page 2 of 2")
	plates := func(body string) int { return strings.Count(body, `<div class="plate">`) }
	_, body = f.get(t, "/captures?page_no=1&sat=NOAA+19&min_elev=60&daynight=day")
	mustContain(t, body, "Page 1 of 1", "Clear", `value="NOAA 19" selected`, `value="day" selected`, `value="60" selected`)
	if plates(body) != 1 || strings.Contains(body, "/images/thumb/M-MCIR.jpg") {
		t.Errorf("satellite/day/elevation filter not applied: %d cards", plates(body))
	}
	_, body = f.get(t, "/captures?page_no=1&daynight=night")
	if plates(body) != 2 || strings.Contains(body, "website-thumbnail.jpg") {
		t.Errorf("day/night filter not applied: %d cards", plates(body))
	}
	// Night imported captures fall back to the MCIR thumbnail rule.
	mustContain(t, body, "/images/thumb/M-MCIR.jpg")
	// Active filters ride along on the pagination links.
	_, body = f.get(t, "/captures?page_no=1&min_elev=20")
	mustContain(t, body, "Page 1 of 2", "?page_no=2&amp;min_elev=20")
}

func TestStatusDocument(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/passes/status?lines=50")
	var doc statusDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("bad json: %v\n%s", err, body)
	}
	if doc.Current != nil {
		t.Error("no capture is running")
	}
	if doc.Next == nil || doc.Next.SatName != "METEOR-M2 4" {
		t.Errorf("next pass = %+v", doc.Next)
	}
	if doc.Latest == nil || doc.Latest.SatName != "NOAA 19" || doc.Latest.MaxSNR == nil {
		t.Errorf("latest = %+v", doc.Latest)
	}
	byName := map[string]schedRow{}
	for _, p := range doc.Passes {
		byName[p.SatName] = p
	}
	if _, ok := byName["NOAA 15"]; ok {
		t.Error("cancelled pass leaked into the schedule")
	}
	if r := byName["NOAA 19"]; r.ID == nil || *r.ID != f.decoded || r.Status != nil || r.IsActive != 0 || r.Direction != "Northbound" {
		t.Errorf("decoded row = %+v", r)
	}
	if r := byName["METEOR-M2 3"]; r.Status == nil || *r.Status != "failed" || r.ErrorText == nil || r.FramesReceived == nil {
		t.Errorf("failed row = %+v", r)
	}
	if r := byName["METEOR-M2 4"]; r.IsActive != 1 || r.Status != nil || r.AzimuthAtMax == nil || *r.AzimuthAtMax != 100 {
		t.Errorf("scheduled row = %+v", r)
	}
	if r := byName["NOAA 18"]; r.Status == nil || *r.Status != "skipped" || r.IsActive != 0 {
		t.Errorf("skipped row = %+v", r)
	}
	if doc.Today != time.Now().Format(dateKeyLayout) {
		t.Errorf("today = %q", doc.Today)
	}
}

func TestStatusIncludesLogWhileCapturing(t *testing.T) {
	f := newFixture(t)
	f.store.SetPassState(f.scheduled, store.StateCapturing, "")
	f.store.DB.Exec(`UPDATE passes SET start_ts = ? WHERE id = ?`, f.now.Unix()-60, f.scheduled)
	f.live.Reset(f.scheduled)
	f.live.Publish("SNR: 7.5 dB")
	_, body := f.get(t, "/passes/status?lines=50")
	var doc statusDoc
	json.Unmarshal([]byte(body), &doc)
	if doc.Current == nil || doc.Current.Status != "capturing" {
		t.Fatalf("current = %+v", doc.Current)
	}
	if len(doc.LogTail) != 1 || doc.LogTail[0] != "SNR: 7.5 dB" || doc.LogPassID != f.scheduled || doc.LogSeq != 1 {
		t.Errorf("log tail = %v (pass %d, seq %d)", doc.LogTail, doc.LogPassID, doc.LogSeq)
	}
}

func TestEventStream(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", f.srv.URL+"/passes/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	rd := bufio.NewReader(res.Body)
	readEvent := func() (string, string) {
		var event, data string
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v", err)
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				event = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			case line == "":
				return event, data
			}
		}
	}
	event, data := readEvent()
	if event != "status" || !strings.Contains(data, `"passes"`) {
		t.Fatalf("first event = %s %s", event, data)
	}
	f.live.Publish("Progress 42%, SNR: 9.1 dB")
	event, data = readEvent()
	if event != "log" || data != `{"seq":1,"line":"Progress 42%, SNR: 9.1 dB"}` {
		t.Fatalf("log event = %s %s", event, data)
	}
}

func TestEmptyGalleryHasOnePage(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/captures?page_no=1&sat=NOWHERE")
	mustContain(t, body, "Page 1 of 1")
	if strings.Contains(body, "of 0") {
		t.Error("empty gallery must not read 'of 0'")
	}
	f.store.DB.Exec(`DELETE FROM passes`)
	_, body = f.get(t, "/admin/captures")
	mustContain(t, body, "Page 1 of 1")
}

func TestLogoutRequiresCSRF(t *testing.T) {
	f := newFixture(t)
	cfg := *f.prov.Get()
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	cfg.Web.Admin = config.WebAdmin{Enabled: true, Username: "admin", PasswordHash: string(hash)}
	f.prov.Set(&cfg)
	c := adminClient(t)
	c.PostForm(f.srv.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	res, _ := c.PostForm(f.srv.URL+"/admin/logout", url.Values{}) // cross-site style: no token
	if res.StatusCode != 403 {
		t.Errorf("logout without csrf: %d", res.StatusCode)
	}
	if res, _ := c.Get(f.srv.URL + "/admin/passes"); res.StatusCode != 200 {
		t.Errorf("session was cleared by a token-less logout: %d", res.StatusCode)
	}
}

func TestAPIParity(t *testing.T) {
	f := newFixture(t)
	_, body := f.get(t, "/api/passes")
	var passes struct {
		ServerTime int64     `json:"server_time"`
		Passes     []apiPass `json:"passes"`
	}
	if err := json.Unmarshal([]byte(body), &passes); err != nil {
		t.Fatal(err)
	}
	if len(passes.Passes) != 2 { // scheduled + skipped; cancelled hidden, past ones excluded
		t.Errorf("api passes = %+v", passes.Passes)
	}

	_, body = f.get(t, "/api/captures?limit=5")
	var caps struct {
		Captures []apiCapture `json:"captures"`
	}
	json.Unmarshal([]byte(body), &caps)
	if len(caps.Captures) != 1 || caps.Captures[0].SatType != 1 || caps.Captures[0].DaylightPass != 1 ||
		!strings.HasSuffix(caps.Captures[0].PageURL, "/captures/listImages?pass_id="+itoa(f.decoded)) {
		t.Errorf("api captures = %+v", caps.Captures)
	}

	_, body = f.get(t, "/api/capture?id="+itoa(f.decoded))
	var one apiCapture
	json.Unmarshal([]byte(body), &one)
	if one.FilePath != "NOAA-19-20260901-120000" || len(one.Images) != 3 || !strings.Contains(one.Images[0], "/images/NOAA-19-20260901-120000-MCIR.jpg") {
		t.Errorf("api capture = %+v", one)
	}
	if status, _ := f.get(t, "/api/capture?id=999"); status != 404 {
		t.Errorf("unknown capture: %d", status)
	}
	if status, _ := f.get(t, "/api/capture"); status != 400 {
		t.Errorf("missing id: %d", status)
	}

	_, body = f.get(t, "/api/status")
	mustContain(t, body, `"current":null`, `"next":{"sat_name":"METEOR-M2 4"`)

	status, body := f.get(t, "/api/rss")
	if status != 200 || !strings.HasPrefix(body, "<?xml") {
		t.Fatalf("rss: %d %s", status, body)
	}
	mustContain(t, body, "<rss version=\"2.0\">", "NOAA 19 - ", "peak SNR 12.3 dB",
		`<enclosure url="`+f.srv.URL+`/images/thumb/NOAA-19-20260901-120000-website-thumbnail.jpg" length="1" type="image/jpeg">`)
}

// adminClient keeps cookies so the CSRF token pairs with its session.
func adminClient(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="csrf" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("no csrf token in page")
	}
	rest := body[i+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

func TestAdminDeletePassRequiresCSRFAndCancels(t *testing.T) {
	f := newFixture(t)
	c := adminClient(t)
	res, _ := c.Get(f.srv.URL + "/admin/passes")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	token := csrfFrom(t, string(body))

	// GET is not a delete, and a POST without the token is refused.
	if res, _ := c.Get(f.srv.URL + "/admin/deletePass?id=" + itoa(f.scheduled)); res.StatusCode != 405 {
		t.Errorf("GET delete: %d", res.StatusCode)
	}
	res, _ = c.PostForm(f.srv.URL+"/admin/deletePass", url.Values{"id": {itoa(f.scheduled)}})
	if res.StatusCode != 403 {
		t.Errorf("POST without csrf: %d", res.StatusCode)
	}
	res, _ = c.PostForm(f.srv.URL+"/admin/deletePass", url.Values{"id": {itoa(f.scheduled)}, "csrf": {token}})
	if res.StatusCode != 303 || !strings.Contains(res.Header.Get("Location"), "status=success") {
		t.Fatalf("delete: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	p, _ := f.store.PassByID(f.scheduled)
	if p.State != store.StateCancelled {
		t.Errorf("pass state = %s, want cancelled", p.State)
	}
	if f.replan.calls != 1 {
		t.Errorf("scheduler replanned %d times, want 1", f.replan.calls)
	}
	// A pass that already ran cannot be "unscheduled".
	res, _ = c.PostForm(f.srv.URL+"/admin/deletePass", url.Values{"id": {itoa(f.decoded)}, "csrf": {token}})
	if !strings.Contains(res.Header.Get("Location"), "Could+not+delete") {
		t.Errorf("deleting a decoded pass must fail: %s", res.Header.Get("Location"))
	}
}

func TestAdminDeleteCaptureRemovesFilesAndRows(t *testing.T) {
	f := newFixture(t)
	c := adminClient(t)
	res, _ := c.Get(f.srv.URL + "/admin/captures")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	token := csrfFrom(t, string(body))

	res, _ = c.PostForm(f.srv.URL+"/admin/deleteCapture", url.Values{"id": {itoa(f.decoded)}, "csrf": {token}, "page_no": {"1"}})
	if res.StatusCode != 303 || !strings.Contains(res.Header.Get("Location"), "status=success") {
		t.Fatalf("delete: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	for _, name := range []string{"NOAA-19-20260901-120000-MCIR.jpg", "NOAA-19-20260901-120000-MSA.jpg", "NOAA-19-20260901-120000-polar-azel.svg"} {
		if _, err := os.Stat(filepath.Join(f.images, name)); !os.IsNotExist(err) {
			t.Errorf("%s still on disk", name)
		}
	}
	for _, name := range []string{"NOAA-19-20260901-120000-MCIR.jpg", "NOAA-19-20260901-120000-website-thumbnail.jpg"} {
		if _, err := os.Stat(filepath.Join(f.thumbs, name)); !os.IsNotExist(err) {
			t.Errorf("thumb %s still on disk", name)
		}
	}
	if p, _ := f.store.PassByID(f.decoded); p != nil {
		t.Error("pass row still present")
	}
	if imgs, _ := f.store.ImagesForPass(f.decoded); len(imgs) != 0 {
		t.Error("image rows still present")
	}
}

func TestAdminLoginFlow(t *testing.T) {
	f := newFixture(t)
	cfg := *f.prov.Get()
	hash, _ := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	cfg.Web.Admin = config.WebAdmin{Enabled: true, Username: "admin", PasswordHash: string(hash)}
	f.prov.Set(&cfg)

	c := adminClient(t)
	res, _ := c.Get(f.srv.URL + "/admin/passes")
	if res.StatusCode != 302 || !strings.HasPrefix(res.Header.Get("Location"), "/admin/login?next=") {
		t.Fatalf("unauthenticated admin: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	res, _ = c.PostForm(f.srv.URL+"/admin/deletePass", url.Values{"id": {"1"}})
	if res.StatusCode != 403 {
		t.Errorf("unauthenticated POST: %d", res.StatusCode)
	}
	res, _ = c.PostForm(f.srv.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"wrong"}, "next": {"/admin/captures"}})
	if res.StatusCode != 401 {
		t.Errorf("bad password: %d", res.StatusCode)
	}
	res, _ = c.PostForm(f.srv.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"s3cret"}, "next": {"https://evil.example/"}})
	if res.StatusCode != 302 || res.Header.Get("Location") != "/admin/passes" {
		t.Fatalf("login: %d %s (open redirect must be neutralised)", res.StatusCode, res.Header.Get("Location"))
	}
	res, _ = c.Get(f.srv.URL + "/admin/passes")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "Log out") {
		t.Fatalf("after login: %d", res.StatusCode)
	}
	token := csrfFrom(t, string(body))
	res, _ = c.PostForm(f.srv.URL+"/admin/deletePass", url.Values{"id": {itoa(f.scheduled)}, "csrf": {token}})
	if res.StatusCode != 303 {
		t.Errorf("authenticated delete: %d", res.StatusCode)
	}
	res, _ = c.PostForm(f.srv.URL+"/admin/logout", url.Values{"csrf": {token}})
	if res.StatusCode != 302 {
		t.Errorf("logout: %d", res.StatusCode)
	}
	if res, _ := c.Get(f.srv.URL + "/admin/passes"); res.StatusCode != 302 {
		t.Errorf("after logout: %d", res.StatusCode)
	}
}

func TestRootRedirectsAndHealth(t *testing.T) {
	f := newFixture(t)
	c := adminClient(t)
	res, _ := c.Get(f.srv.URL + "/")
	if res.StatusCode != 302 || res.Header.Get("Location") != "/passes" {
		t.Errorf("root: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	_, body := f.get(t, "/healthz")
	mustContain(t, body, `"status":"ok"`, `"version":"test"`)
}

func itoa(v int64) string {
	return strings.TrimSpace(strings.Replace(json.Number(itoaRaw(v)).String(), " ", "", -1))
}

func itoaRaw(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
