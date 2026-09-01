// Package notify pushes captures, daily summaries and station alerts to the
// configured channels: a generic JSON webhook, Discord, Telegram, Pushover
// and email — RN2's push_processors ported, minus the social networks.
//
// Every push is best-effort: failures are logged and never affect the
// capture. The quality gate mutes the social channels for weak passes; the
// webhook is exempt because it is an integration event, not a publication.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/perhp/rnv3/internal/config"
)

// PassEvent describes a decoded pass for the push channels.
type PassEvent struct {
	PassID       int64
	Satellite    string
	SatType      config.SatelliteType
	StartTS      int64
	EndTS        int64
	MaxElevation float64
	Direction    string // "Northbound" | "Southbound"
	Side         string // "E" | "W" (side of the sky at max elevation)
	SunElevation float64
	Gain         float64 // 0 = automatic
	Daylight     bool
	MaxSNR       *float64
	AvgSNR       *float64
	Images       []string // absolute paths, production order
}

// requestTimeout bounds every outbound HTTP call.
const requestTimeout = 60 * time.Second

// Notifier holds the channel implementations. Config is snapshotted per
// push, so a reload applies to the next one.
type Notifier struct {
	Prov   *config.Provider
	Client *http.Client
	// SendMail delivers one message; nil means the real SMTP client.
	SendMail func(cfg config.Email, from string, to []string, msg []byte) error
	Log      *slog.Logger
}

func New(prov *config.Provider) *Notifier {
	return &Notifier{Prov: prov, Client: &http.Client{Timeout: requestTimeout}, Log: slog.Default()}
}

func (n *Notifier) log() *slog.Logger {
	if n.Log == nil {
		return slog.Default()
	}
	return n.Log
}

// Annotation is the message text RN2 attached to every per-pass push:
// "[Ground Station: X] SAT 02-09-2026 12:34 CEST Max Elev: 62° W Sun
// Elevation: 30° Gain: 29.7 | Northbound".
func Annotation(cfg *config.Config, ev PassEvent) string {
	var parts []string
	if cfg.Station.Location != "" {
		parts = append(parts, "Ground Station: "+cfg.Station.Location)
	}
	parts = append(parts,
		ev.Satellite+" "+captureStart(ev),
		fmt.Sprintf("Max Elev: %.0f° %s", ev.MaxElevation, ev.Side),
		fmt.Sprintf("Sun Elevation: %.0f°", ev.SunElevation),
		"Gain: "+gainLabel(ev.Gain),
		"| "+ev.Direction)
	return strings.Join(parts, " ")
}

func captureStart(ev PassEvent) string {
	return time.Unix(ev.StartTS, 0).Format("02-01-2006 15:04 MST")
}

func gainLabel(g float64) string {
	if g == 0 {
		return "Automatic"
	}
	return fmt.Sprintf("%g", g)
}

// QualityGateReason returns why the social channels are muted for the pass,
// or "" when it passes the gate (or the gate is off). Passes without SNR
// readings are not SNR-gated (RN2 parity).
func QualityGateReason(cfg *config.Config, ev PassEvent) string {
	g := cfg.Notifications.QualityGate
	if !g.Enabled {
		return ""
	}
	if ev.MaxElevation < g.MinElevation {
		return fmt.Sprintf("max elevation %.0f° below %.0f°", ev.MaxElevation, g.MinElevation)
	}
	if ev.MaxSNR != nil && *ev.MaxSNR < g.MinSNR {
		return fmt.Sprintf("peak SNR %.1f dB below %.1f dB", *ev.MaxSNR, g.MinSNR)
	}
	return ""
}

// capturePageURL is the panel link for a capture, built from the station
// name (or the Pushover link base when set).
func capturePageURL(cfg *config.Config, base string, passID int64) string {
	if base == "" {
		base = "http://" + cfg.Station.Name
	}
	return fmt.Sprintf("%s/captures/listImages?pass_id=%d", strings.TrimRight(base, "/"), passID)
}

// PassDecoded pushes a finished capture through every enabled channel.
func (n *Notifier) PassDecoded(ctx context.Context, ev PassEvent) {
	cfg := n.Prov.Get()
	nc := cfg.Notifications
	log := n.log().With("pass_id", ev.PassID, "satellite", ev.Satellite)

	if nc.Webhook.Enabled {
		if err := n.webhookCapture(ctx, cfg, ev); err != nil {
			log.Warn("webhook push failed", "err", err)
		}
	}
	if reason := QualityGateReason(cfg, ev); reason != "" {
		log.Info("push quality gate: skipping social pushes for this pass", "reason", reason)
		return
	}
	annotation := Annotation(cfg, ev)

	if nc.Pushover.Enabled {
		msg := fmt.Sprintf("<b>Start: </b>%s<br/><b>Max Elev: </b>%.0f° %s<br/> <a href=%s>BROWSER LINK</a>",
			captureStart(ev), ev.MaxElevation, ev.Side, capturePageURL(cfg, nc.Pushover.LinkURL, ev.PassID))
		if err := n.pushover(ctx, nc.Pushover, ev.Satellite, msg, true, pushoverAttachment(ev.Images)); err != nil {
			log.Warn("pushover push failed", "err", err)
		}
	}
	if nc.Telegram.Enabled {
		if err := n.telegramPhotos(ctx, nc.Telegram, annotation, ev.Images); err != nil {
			log.Warn("telegram push failed", "err", err)
		}
	}
	if nc.Email.Enabled {
		for _, img := range ev.Images {
			if err := n.email(cfg.Notifications.Email, annotation, img); err != nil {
				log.Warn("email push failed", "image", filepath.Base(img), "err", err)
			}
		}
	}
	if nc.Discord.Enabled {
		url := nc.Discord.NOAAWebhookURL
		if ev.SatType == config.SatMeteorLRPT {
			url = nc.Discord.MeteorWebhookURL
		}
		if url == "" {
			log.Warn("discord push skipped: no webhook url for this satellite type")
		} else {
			for _, img := range ev.Images {
				if err := n.discordFile(ctx, url, img, annotation); err != nil {
					log.Warn("discord push failed", "image", filepath.Base(img), "err", err)
				}
			}
		}
	}
}

// Alert sends a station health alert (watchdog) through the text-capable
// channels: Telegram, Discord, Pushover and the webhook (event
// "station_alert").
func (n *Notifier) Alert(ctx context.Context, check, message string) {
	cfg := n.Prov.Get()
	nc := cfg.Notifications
	full := "Station alert"
	if cfg.Station.Location != "" {
		full += " (" + cfg.Station.Location + ")"
	}
	full += ": " + message
	log := n.log().With("check", check)
	log.Error("watchdog: " + full)

	if nc.Telegram.Enabled {
		if err := n.telegramText(ctx, nc.Telegram, full); err != nil {
			log.Warn("telegram alert failed", "err", err)
		}
	}
	if nc.Discord.Enabled {
		url := nc.Discord.NOAAWebhookURL
		if url == "" {
			url = nc.Discord.MeteorWebhookURL
		}
		if err := n.discordText(ctx, url, full); err != nil {
			log.Warn("discord alert failed", "err", err)
		}
	}
	if nc.Pushover.Enabled {
		if err := n.pushover(ctx, nc.Pushover, "", full, false, ""); err != nil {
			log.Warn("pushover alert failed", "err", err)
		}
	}
	if nc.Webhook.Enabled {
		if err := n.webhook(ctx, nc.Webhook, map[string]any{"event": "station_alert", "check": check, "message": full}); err != nil {
			log.Warn("webhook alert failed", "err", err)
		}
	}
}

// DailySummary pushes the best-of-day post: annotation plus the day's
// representative image, timelapses and mosaics.
func (n *Notifier) DailySummary(ctx context.Context, annotation string, files []string) {
	nc := n.Prov.Get().Notifications
	log := n.log()
	if nc.Telegram.Enabled {
		if err := n.telegramPhotos(ctx, nc.Telegram, annotation, files); err != nil {
			log.Warn("telegram daily summary failed", "err", err)
		}
	}
	if nc.Discord.Enabled {
		url := nc.Discord.NOAAWebhookURL
		if url == "" {
			url = nc.Discord.MeteorWebhookURL
		}
		for _, f := range files {
			if err := n.discordFile(ctx, url, f, annotation); err != nil {
				log.Warn("discord daily summary failed", "file", filepath.Base(f), "err", err)
			}
		}
	}
	if nc.Pushover.Enabled {
		if err := n.pushover(ctx, nc.Pushover, "Best of day", annotation, false, pushoverAttachment(files)); err != nil {
			log.Warn("pushover daily summary failed", "err", err)
		}
	}
	if nc.Email.Enabled {
		for _, f := range files {
			if err := n.email(nc.Email, annotation, f); err != nil {
				log.Warn("email daily summary failed", "file", filepath.Base(f), "err", err)
			}
		}
	}
}

// ContributeCADU uploads a Meteor CADU recording to the community composite
// service (RN2: curl -F file=@x.cadu $URL/meteor). Recordings are tens of
// megabytes on a small-memory station, so the body is streamed from disk
// rather than buffered, and the deadline is the caller's ctx — not the
// per-request timeout the notification calls use.
func (n *Notifier) ContributeCADU(ctx context.Context, path string) error {
	cfg := n.Prov.Get()
	if !cfg.Community.ContributeComposites {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", filepath.Base(path))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.Community.URL, "/")+"/meteor", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := n.streamClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	n.log().Info("contributed CADU to community composites", "file", filepath.Base(path), "status", res.StatusCode)
	return nil
}

// streamClient is the transport for long uploads: no whole-request timeout,
// the caller's context bounds it.
func (n *Notifier) streamClient() *http.Client {
	c := &http.Client{}
	if n.Client != nil {
		c.Transport = n.Client.Transport
	}
	return c
}

// ---- channels ----------------------------------------------------------------

func (n *Notifier) webhookCapture(ctx context.Context, cfg *config.Config, ev PassEvent) error {
	payload := map[string]any{
		"event":            "capture_complete",
		"satellite":        ev.Satellite,
		"pass_id":          ev.PassID,
		"pass_start":       ev.StartTS,
		"pass_end":         ev.EndTS,
		"duration_seconds": ev.EndTS - ev.StartTS,
		"max_elevation":    ev.MaxElevation,
		"pass_direction":   ev.Direction,
		"pass_side":        ev.Side,
		"sun_elevation":    ev.SunElevation,
		"gain":             ev.Gain,
		"daylight_pass":    ev.Daylight,
		"ground_station":   cfg.Station.Location,
		"images":           ev.Images,
		"max_snr":          ev.MaxSNR,
		"avg_snr":          ev.AvgSNR,
		"page_url":         capturePageURL(cfg, "", ev.PassID),
	}
	return n.webhook(ctx, cfg.Notifications.Webhook, payload)
}

func (n *Notifier) webhook(ctx context.Context, w config.Webhook, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.AuthToken)
	}
	_, err = n.send(req)
	return err
}

func (n *Notifier) discordFile(ctx context.Context, url, file, content string) error {
	payload, _ := json.Marshal(map[string]string{"content": content})
	form, err := multipartFile("file", file, map[string]string{"payload_json": string(payload)})
	if err != nil {
		return err
	}
	_, err = n.do(ctx, url, form)
	return err
}

func (n *Notifier) discordText(ctx context.Context, url, content string) error {
	body, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = n.send(req)
	return err
}

// telegramAPI is overridable in tests.
var telegramAPI = "https://api.telegram.org"

// telegramPhotos sends each file; the caption goes on the first one only so
// an album of enhancements doesn't repeat the text. GIFs go as documents,
// which Telegram plays inline; sendPhoto rejects them.
func (n *Notifier) telegramPhotos(ctx context.Context, t config.Telegram, caption string, files []string) error {
	var firstErr error
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			n.log().Warn("telegram: image missing, skipping", "file", f)
			continue
		}
		method, field := "sendPhoto", "photo"
		if strings.EqualFold(filepath.Ext(f), ".gif") {
			method, field = "sendDocument", "document"
		}
		form, err := multipartFile(field, f, map[string]string{"chat_id": t.ChatID, "caption": caption})
		if err != nil {
			return err
		}
		if _, err := n.do(ctx, fmt.Sprintf("%s/bot%s/%s", telegramAPI, t.BotToken, method), form); err != nil && firstErr == nil {
			firstErr = err
		}
		caption = ""
	}
	return firstErr
}

func (n *Notifier) telegramText(ctx context.Context, t config.Telegram, text string) error {
	form, err := multipartFields(map[string]string{"chat_id": t.ChatID, "text": text})
	if err != nil {
		return err
	}
	_, err = n.do(ctx, fmt.Sprintf("%s/bot%s/sendMessage", telegramAPI, t.BotToken), form)
	return err
}

// pushoverAPI is overridable in tests.
var pushoverAPI = "https://api.pushover.net/1/messages.json"

// pushoverMaxAttachment is Pushover's attachment size limit.
const pushoverMaxAttachment = 2621440

// pushoverAttachment picks the image RN2's push_pushover.sh would: the
// 221_corrected composite, else MSA, else the last MCIR.
func pushoverAttachment(files []string) string {
	pick := ""
	for _, f := range files {
		base := filepath.Base(f)
		if strings.Contains(base, "-221_corrected") {
			return f
		}
		if strings.Contains(base, "MSA") {
			return f
		}
		if strings.Contains(base, "MCIR") {
			pick = f
		}
	}
	return pick
}

func (n *Notifier) pushover(ctx context.Context, p config.Pushover, title, message string, html bool, attachment string) error {
	fields := map[string]string{
		"token": p.APIToken, "user": p.User, "message": message, "priority": fmt.Sprint(p.Priority),
	}
	if title != "" {
		fields["title"] = title
	}
	if html {
		fields["html"] = "1"
	}
	var form *formBody
	var err error
	if attachment != "" {
		data, rerr := attachmentBytes(attachment, pushoverMaxAttachment)
		if rerr != nil {
			n.log().Warn("pushover: no usable attachment", "file", attachment, "err", rerr)
			form, err = multipartFields(fields)
		} else {
			form, err = multipartBytes("attachment", filepath.Base(attachment), data, fields)
		}
	} else {
		form, err = multipartFields(fields)
	}
	if err != nil {
		return err
	}
	_, err = n.do(ctx, pushoverAPI, form)
	return err
}

// ---- transport ---------------------------------------------------------------

type formBody struct {
	buf         bytes.Buffer
	contentType string
}

func multipartFile(field, path string, fields map[string]string) (*formBody, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return multipartBytes(field, filepath.Base(path), data, fields)
}

func multipartBytes(field, filename string, data []byte, fields map[string]string) (*formBody, error) {
	fb := &formBody{}
	w := multipart.NewWriter(&fb.buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	fb.contentType = w.FormDataContentType()
	return fb, nil
}

func multipartFields(fields map[string]string) (*formBody, error) {
	fb := &formBody{}
	w := multipart.NewWriter(&fb.buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	fb.contentType = w.FormDataContentType()
	return fb, nil
}

func (n *Notifier) do(ctx context.Context, url string, form *formBody) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &form.buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", form.contentType)
	return n.send(req)
}

// send performs the request and treats any non-2xx as an error carrying the
// start of the response body (the APIs explain themselves there).
func (n *Notifier) send(req *http.Request) (int, error) {
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	return res.StatusCode, nil
}

// ---- email -------------------------------------------------------------------

// email sends one message with the image attached (RN2: mpack via msmtp,
// one mail per image, subject = annotation).
func (n *Notifier) email(e config.Email, subject, attachment string) error {
	data, err := os.ReadFile(attachment)
	if err != nil {
		return err
	}
	msg := buildEmail(e.From, e.To, subject, subject, filepath.Base(attachment), data)
	send := n.SendMail
	if send == nil {
		send = smtpSend
	}
	return send(e, e.From, []string{e.To}, msg)
}

// smtpSend delivers via SMTP: implicit TLS on port 465, otherwise STARTTLS
// when the server offers it, PLAIN auth when a user is configured.
func smtpSend(e config.Email, from string, to []string, msg []byte) error {
	addr := fmt.Sprintf("%s:%d", e.SMTPHost, e.SMTPPort)
	var auth smtp.Auth
	if e.SMTPUser != "" {
		auth = smtp.PlainAuth("", e.SMTPUser, e.SMTPPassword, e.SMTPHost)
	}
	if e.SMTPPort != 465 {
		return smtp.SendMail(addr, auth, from, to, msg)
	}
	return smtpSendTLS(addr, e.SMTPHost, auth, from, to, msg)
}
