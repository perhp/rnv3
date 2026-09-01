package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
)

// call is one request the fake endpoint received.
type call struct {
	Path   string
	JSON   map[string]any
	Fields map[string]string
	Files  map[string]int // form field → attachment size
	Auth   string
}

type endpoint struct {
	mu    sync.Mutex
	calls []call
	srv   *httptest.Server
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{Path: r.URL.Path, Fields: map[string]string{}, Files: map[string]int{}, Auth: r.Header.Get("Authorization")}
		ct := r.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "application/json"):
			json.NewDecoder(r.Body).Decode(&c.JSON)
		case strings.HasPrefix(ct, "multipart/form-data"):
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Errorf("bad multipart: %v", err)
			}
			for k, v := range r.MultipartForm.Value {
				c.Fields[k] = v[0]
			}
			for k, v := range r.MultipartForm.File {
				f, _ := v[0].Open()
				data, _ := io.ReadAll(f)
				c.Files[k] = len(data)
			}
		}
		e.mu.Lock()
		e.calls = append(e.calls, c)
		e.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *endpoint) byPath(prefix string) []call {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []call
	for _, c := range e.calls {
		if strings.HasPrefix(c.Path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
		}
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	jpeg.Encode(f, img, &jpeg.Options{Quality: 95})
}

type mail struct {
	to      []string
	subject string
	msg     []byte
}

func testNotifier(t *testing.T, e *endpoint) (*Notifier, *config.Config, *[]mail) {
	t.Helper()
	cfg := config.Default()
	cfg.Station.Location = "Copenhagen"
	cfg.Station.Name = "raspinoaa.local"
	cfg.Notifications = config.Notifications{
		Webhook:  config.Webhook{Enabled: true, URL: e.srv.URL + "/hook", AuthToken: "tok"},
		Discord:  config.Discord{Enabled: true, NOAAWebhookURL: e.srv.URL + "/discord/noaa", MeteorWebhookURL: e.srv.URL + "/discord/meteor"},
		Telegram: config.Telegram{Enabled: true, BotToken: "BOT", ChatID: "42"},
		Pushover: config.Pushover{Enabled: true, APIToken: "PO", User: "U", Priority: 1},
		Email:    config.Email{Enabled: true, To: "me@example.org", From: "station@example.org", SMTPHost: "smtp.example.org", SMTPPort: 587},
	}
	telegramAPI = e.srv.URL + "/tg"
	pushoverAPI = e.srv.URL + "/pushover"
	mails := &[]mail{}
	n := New(config.NewProvider(cfg))
	n.SendMail = func(ec config.Email, from string, to []string, msg []byte) error {
		subj := ""
		for _, line := range strings.Split(string(msg), "\r\n") {
			if strings.HasPrefix(line, "Subject: ") {
				subj = strings.TrimPrefix(line, "Subject: ")
			}
		}
		*mails = append(*mails, mail{to: to, subject: subj, msg: msg})
		return nil
	}
	return n, cfg, mails
}

func testEvent(t *testing.T, dir string, sat string, typ config.SatelliteType) PassEvent {
	t.Helper()
	snr := 12.3
	ev := PassEvent{
		PassID: 77, Satellite: sat, SatType: typ, StartTS: time.Date(2026, 9, 2, 12, 34, 0, 0, time.Local).Unix(),
		MaxElevation: 62.4, Direction: "Northbound", Side: "W", SunElevation: 30.2, Gain: 29.7, Daylight: true, MaxSNR: &snr,
	}
	ev.EndTS = ev.StartTS + 600
	for _, k := range []string{"MCIR", "MSA", "HVC"} {
		p := filepath.Join(dir, "NOAA-19-x-"+k+".jpg")
		writeJPEG(t, p, 32, 32)
		ev.Images = append(ev.Images, p)
	}
	return ev
}

func TestAnnotationAndQualityGate(t *testing.T) {
	cfg := config.Default()
	cfg.Station.Location = "Copenhagen"
	ev := testEvent(t, t.TempDir(), "NOAA 19", config.SatNOAAAPT)
	want := "Ground Station: Copenhagen NOAA 19 " + time.Unix(ev.StartTS, 0).Format("02-01-2006 15:04 MST") +
		" Max Elev: 62° W Sun Elevation: 30° Gain: 29.7 | Northbound"
	if got := Annotation(cfg, ev); got != want {
		t.Errorf("annotation\n got %q\nwant %q", got, want)
	}
	ev.Gain = 0
	cfg.Station.Location = ""
	if got := Annotation(cfg, ev); !strings.HasPrefix(got, "NOAA 19 ") || !strings.Contains(got, "Gain: Automatic") {
		t.Errorf("annotation without location / auto gain = %q", got)
	}

	if r := QualityGateReason(cfg, ev); r != "" {
		t.Errorf("gate off but reason %q", r)
	}
	cfg.Notifications.QualityGate = config.QualityGate{Enabled: true, MinElevation: 70}
	if r := QualityGateReason(cfg, ev); !strings.Contains(r, "max elevation 62° below 70°") {
		t.Errorf("elevation gate reason = %q", r)
	}
	cfg.Notifications.QualityGate = config.QualityGate{Enabled: true, MinSNR: 15}
	if r := QualityGateReason(cfg, ev); !strings.Contains(r, "peak SNR 12.3 dB below 15.0 dB") {
		t.Errorf("snr gate reason = %q", r)
	}
	ev.MaxSNR = nil // no readings: not gated
	if r := QualityGateReason(cfg, ev); r != "" {
		t.Errorf("pass without SNR must not be SNR-gated: %q", r)
	}
}

func TestPassDecodedPushesEveryChannel(t *testing.T) {
	e := newEndpoint(t)
	n, cfg, mails := testNotifier(t, e)
	ev := testEvent(t, t.TempDir(), "METEOR-M2 3", config.SatMeteorLRPT)
	n.PassDecoded(context.Background(), ev)
	annotation := Annotation(cfg, ev)

	hooks := e.byPath("/hook")
	if len(hooks) != 1 || hooks[0].Auth != "Bearer tok" {
		t.Fatalf("webhook calls = %+v", hooks)
	}
	h := hooks[0].JSON
	if h["event"] != "capture_complete" || h["satellite"] != "METEOR-M2 3" || h["pass_side"] != "W" ||
		h["daylight_pass"] != true || h["ground_station"] != "Copenhagen" || h["duration_seconds"] != float64(600) ||
		h["max_snr"] != 12.3 || h["page_url"] != "http://raspinoaa.local/captures/listImages?pass_id=77" {
		t.Errorf("webhook payload = %v", h)
	}
	if imgs, _ := h["images"].([]any); len(imgs) != 3 {
		t.Errorf("webhook images = %v", h["images"])
	}

	// Discord: one multipart per image, to the Meteor webhook.
	if d := e.byPath("/discord/meteor"); len(d) != 3 || d[0].Files["file"] == 0 || !strings.Contains(d[0].Fields["payload_json"], annotation) {
		t.Errorf("discord calls = %+v", d)
	}
	if d := e.byPath("/discord/noaa"); len(d) != 0 {
		t.Error("meteor pass must not hit the NOAA webhook")
	}
	// Telegram: caption on the first photo only.
	tg := e.byPath("/tg/botBOT/sendPhoto")
	if len(tg) != 3 || tg[0].Fields["caption"] != annotation || tg[1].Fields["caption"] != "" || tg[0].Fields["chat_id"] != "42" {
		t.Errorf("telegram calls = %+v", tg)
	}
	// Pushover: html message with the browser link, MSA attachment preferred over MCIR.
	po := e.byPath("/pushover")
	if len(po) != 1 || po[0].Fields["title"] != "METEOR-M2 3" || po[0].Fields["html"] != "1" || po[0].Fields["priority"] != "1" ||
		!strings.Contains(po[0].Fields["message"], "<a href=http://raspinoaa.local/captures/listImages?pass_id=77>BROWSER LINK</a>") ||
		po[0].Files["attachment"] == 0 {
		t.Errorf("pushover calls = %+v", po)
	}
	// Email: one per image, subject = annotation, base64 attachment inside.
	if len(*mails) != 3 || (*mails)[0].to[0] != "me@example.org" || !strings.Contains((*mails)[0].subject, "METEOR-M2") ||
		!bytes.Contains((*mails)[0].msg, []byte("Content-Transfer-Encoding: base64")) ||
		!bytes.Contains((*mails)[0].msg, []byte(`filename="NOAA-19-x-MCIR.jpg"`)) {
		t.Errorf("mails = %d %+v", len(*mails), *mails)
	}
}

func TestQualityGateMutesSocialButNotWebhook(t *testing.T) {
	e := newEndpoint(t)
	n, cfg, mails := testNotifier(t, e)
	cfg.Notifications.QualityGate = config.QualityGate{Enabled: true, MinElevation: 80}
	n.Prov.Set(cfg)
	n.PassDecoded(context.Background(), testEvent(t, t.TempDir(), "NOAA 19", config.SatNOAAAPT))
	if len(e.byPath("/hook")) != 1 {
		t.Error("webhook must fire regardless of the gate")
	}
	if len(e.calls) != 1 || len(*mails) != 0 {
		t.Errorf("social channels must be muted: %d calls, %d mails", len(e.calls), len(*mails))
	}
}

func TestPushoverAttachmentPreference(t *testing.T) {
	files := []string{"/x/A-MCIR.jpg", "/x/A-HVC.jpg", "/x/A-MSA.jpg", "/x/A-221_corrected.jpg"}
	if got := pushoverAttachment(files); got != "/x/A-MSA.jpg" {
		t.Errorf("MSA should win before reaching 221 later in the list: %s", got)
	}
	if got := pushoverAttachment([]string{"/x/A-221_corrected.jpg", "/x/A-MSA.jpg"}); got != "/x/A-221_corrected.jpg" {
		t.Errorf("221_corrected first: %s", got)
	}
	if got := pushoverAttachment([]string{"/x/A-HVC.jpg", "/x/A-MCIR.jpg", "/x/A-ZA.jpg"}); got != "/x/A-MCIR.jpg" {
		t.Errorf("MCIR fallback: %s", got)
	}
	if got := pushoverAttachment([]string{"/x/A-ZA.jpg"}); got != "" {
		t.Errorf("no candidate: %q", got)
	}
}

func TestAlertAndDailySummary(t *testing.T) {
	e := newEndpoint(t)
	n, _, mails := testNotifier(t, e)
	n.Alert(context.Background(), "disk_usage", "image storage is 95% full")
	full := "Station alert (Copenhagen): image storage is 95% full"
	if tg := e.byPath("/tg/botBOT/sendMessage"); len(tg) != 1 || tg[0].Fields["text"] != full {
		t.Errorf("telegram alert = %+v", tg)
	}
	if d := e.byPath("/discord/noaa"); len(d) != 1 || d[0].JSON["content"] != full {
		t.Errorf("discord alert = %+v", d)
	}
	if po := e.byPath("/pushover"); len(po) != 1 || po[0].Fields["message"] != full || po[0].Fields["html"] != "" {
		t.Errorf("pushover alert = %+v", po)
	}
	if h := e.byPath("/hook"); len(h) != 1 || h[0].JSON["event"] != "station_alert" || h[0].JSON["check"] != "disk_usage" {
		t.Errorf("webhook alert = %+v", h)
	}
	if len(*mails) != 0 {
		t.Error("alerts are not emailed (RN2 parity)")
	}

	dir := t.TempDir()
	best := filepath.Join(dir, "NOAA-19-x-MSA.jpg")
	writeJPEG(t, best, 16, 16)
	gif := filepath.Join(dir, "timelapse-20260902-321_projected.gif")
	os.WriteFile(gif, []byte("GIF89a"), 0o644)
	n.DailySummary(context.Background(), "Daily summary 2026-09-02", []string{best, gif})
	if p := e.byPath("/tg/botBOT/sendPhoto"); len(p) != 1 || p[0].Fields["caption"] != "Daily summary 2026-09-02" {
		t.Errorf("telegram summary photo = %+v", p)
	}
	if d := e.byPath("/tg/botBOT/sendDocument"); len(d) != 1 || d[0].Files["document"] == 0 || d[0].Fields["caption"] != "" {
		t.Errorf("gif must go as a document without repeating the caption: %+v", d)
	}
	if po := e.byPath("/pushover"); len(po) != 2 || po[1].Fields["title"] != "Best of day" || po[1].Files["attachment"] == 0 {
		t.Errorf("pushover summary = %+v", po)
	}
	if len(*mails) != 2 {
		t.Errorf("summary mails = %d, want one per file", len(*mails))
	}
}

func TestContributeCADU(t *testing.T) {
	e := newEndpoint(t)
	n, cfg, _ := testNotifier(t, e)
	cadu := filepath.Join(t.TempDir(), "x.cadu")
	os.WriteFile(cadu, bytes.Repeat([]byte{1}, 4096), 0o644)
	if err := n.ContributeCADU(context.Background(), cadu); err != nil {
		t.Fatal(err)
	}
	if len(e.calls) != 0 {
		t.Fatal("disabled in config: nothing must be uploaded")
	}
	cfg.Community = config.Community{ContributeComposites: true, URL: e.srv.URL + "/upload/"}
	n.Prov.Set(cfg)
	if err := n.ContributeCADU(context.Background(), cadu); err != nil {
		t.Fatal(err)
	}
	if u := e.byPath("/upload/meteor"); len(u) != 1 || u[0].Files["file"] != 4096 {
		t.Errorf("upload = %+v", u)
	}
}

func TestAttachmentBytesShrinksOversizedJPEG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jpg")
	writeJPEG(t, path, 600, 600) // noise: well over 50 KB
	info, _ := os.Stat(path)
	limit := int(info.Size() / 3)
	data, err := attachmentBytes(path, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > limit {
		t.Errorf("shrunk to %d bytes, limit %d", len(data), limit)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() >= 600 {
		t.Errorf("result not a smaller JPEG: %v %v", err, img.Bounds())
	}
	small, err := attachmentBytes(path, int(info.Size()))
	if err != nil || len(small) != int(info.Size()) {
		t.Error("a file within the limit must be returned untouched")
	}
	txt := filepath.Join(t.TempDir(), "x.gif")
	os.WriteFile(txt, bytes.Repeat([]byte{1}, 100), 0o644)
	if _, err := attachmentBytes(txt, 10); err == nil {
		t.Error("non-JPEG over the limit must be refused, not silently truncated")
	}
}

func TestBuildEmail(t *testing.T) {
	msg := string(buildEmail("a@x", "b@y", "Subj ünïcode", "body", "img.jpg", []byte("JPEGDATA")))
	for _, want := range []string{"From: a@x\r\n", "To: b@y\r\n", "Subject: =?utf-8?q?", "multipart/mixed", "Content-Type: image/jpeg; name=\"img.jpg\"",
		"Content-Disposition: attachment; filename=\"img.jpg\"", "SlBFR0RBVEE=", "body\r\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("email missing %q\n%s", want, msg)
		}
	}
}
