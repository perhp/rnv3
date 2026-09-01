package setup

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/perhp/rnv3/internal/config"
)

// scripted builds a prompter fed from canned input lines.
func scripted(answers map[string]string, lines ...string) (*Prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	input := ""
	if len(lines) > 0 {
		input = strings.Join(lines, "\n") + "\n"
	}
	p := &Prompter{In: bufio.NewReader(strings.NewReader(input)), Out: out, Answers: answers, Used: map[string]string{},
		Fatal: func(err error) { panic("prompter fatal: " + err.Error()) }}
	return p, out
}

func TestPrompterBasics(t *testing.T) {
	p, out := scripted(map[string]string{"pre": "canned"},
		"",        // Ask keeps default
		"typed",   // Ask override
		"maybe",   // bad bool
		"y",       // bool
		"x", "12", // int retry
		"2",       // choice by number
		"2 3", "", // multi: toggle 2 and 3, confirm
		"", "sekrit", // secret: unchanged then value
	)
	if v := p.Ask("pre", "Pre", "d"); v != "canned" {
		t.Errorf("answers file not used: %q", v)
	}
	if v := p.Ask("a", "A", "dflt"); v != "dflt" {
		t.Errorf("default not kept: %q", v)
	}
	if v := p.Ask("b", "B", "dflt"); v != "typed" {
		t.Errorf("typed value lost: %q", v)
	}
	if !p.AskBool("c", "C", false) {
		t.Error("y not accepted after a retry")
	}
	if v := p.AskInt("d", "D", 5); v != 12 {
		t.Errorf("int = %d", v)
	}
	if v := p.AskChoice("e", "E", []string{"one", "two", "three"}, "one"); v != "two" {
		t.Errorf("choice = %q", v)
	}
	sel := p.AskMulti("f", "F", []string{"a", "b", "c"}, map[string]bool{"a": true})
	if !sel["a"] || !sel["b"] || !sel["c"] {
		t.Errorf("multi = %v", sel)
	}
	if v := p.AskSecret("g", "G", true); v != "" {
		t.Errorf("unchanged secret = %q", v)
	}
	if v := p.AskSecret("h", "H", false); v != "sekrit" {
		t.Errorf("secret = %q", v)
	}
	if p.Used["f"] != "a,b,c" || p.Used["c"] != "true" {
		t.Errorf("used answers = %v", p.Used)
	}
	if !strings.Contains(out.String(), "please answer y or n") || !strings.Contains(out.String(), "whole number") {
		t.Error("validation hints missing")
	}
}

func TestChooseModeDependsOnProbe(t *testing.T) {
	// Bare Pi: only "fresh".
	p, out := scripted(nil, "")
	w := &Wizard{P: p, Probe: &Probe{}}
	if m := w.ChooseMode(); m != ModeFresh {
		t.Errorf("bare pi mode = %s", m)
	}
	if strings.Contains(out.String(), "Cut over") {
		t.Error("cutover offered without rnv3 installed")
	}
	// RN2 present, rnv3 installed: side-by-side is the default, cutover offered.
	p, out = scripted(nil, "")
	w = &Wizard{P: p, Probe: &Probe{RN2Home: "/home/pi/raspberry-noaa-v2", RNV3Version: "rnv3 1", RNV3Config: "x"}}
	if m := w.ChooseMode(); m != ModeSideBySide {
		t.Errorf("mode = %s, want side-by-side default", m)
	}
	if !strings.Contains(out.String(), "Cut over") || !strings.Contains(out.String(), "Reconfigure") {
		t.Error("cutover/reconfigure not offered")
	}
	// Answers file names the mode.
	p, _ = scripted(map[string]string{"mode": "reconfigure"})
	w = &Wizard{P: p, Probe: &Probe{RNV3Config: "x"}}
	if m := w.ChooseMode(); m != ModeReconfigure {
		t.Errorf("mode from answers = %s", m)
	}
}

func TestConfigureEssentialsAndSideBySide(t *testing.T) {
	answers := map[string]string{
		"station.name": "raspinoaa", "station.location": "Copenhagen", "station.latitude": "55.68", "station.longitude": "12.57",
		"station.altitude": "20", "sdr.type": "rtlsdr", "satellites.enabled": "NOAA 19,METEOR-M2 3",
		"web.admin.enabled": "true", "web.admin.username": "admin", "web.admin.password": "hunter22",
		"section.notifications": "false", "section.daily": "false", "section.panel": "false", "section.advanced": "false",
	}
	p, _ := scripted(answers)
	w := &Wizard{P: p, Probe: &Probe{Hostname: "raspinoaa", NginxActive: true}}
	cfg, err := w.Configure(config.Default(), ModeSideBySide)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Station.Latitude != 55.68 || cfg.Station.Location != "Copenhagen" || cfg.SDR.Type != "rtlsdr" {
		t.Errorf("station = %+v sdr = %+v", cfg.Station, cfg.SDR)
	}
	enabled := map[string]bool{}
	for _, s := range cfg.Satellites {
		enabled[s.Name] = s.Enabled
	}
	if !enabled["NOAA 19"] || !enabled["METEOR-M2 3"] || enabled["NOAA 15"] || enabled["METEOR-M2 4"] {
		t.Errorf("enabled = %v", enabled)
	}
	if !cfg.Web.Admin.Enabled || bcrypt.CompareHashAndPassword([]byte(cfg.Web.Admin.PasswordHash), []byte("hunter22")) != nil {
		t.Error("admin password not hashed")
	}
	if !cfg.Scheduling.DryRun || cfg.Web.Listen != ":8080" {
		t.Errorf("side-by-side adjustments: dry_run=%v listen=%s", cfg.Scheduling.DryRun, cfg.Web.Listen)
	}
	// Defaults must not have been mutated.
	if config.Default().Satellites[2].Enabled {
		t.Error("Default() satellites mutated by Configure")
	}

	// Fresh mode with nothing enabled is refused.
	answers["satellites.enabled"] = ""
	p, _ = scripted(answers)
	w = &Wizard{P: p, Probe: &Probe{Hostname: "raspinoaa"}}
	if _, err := w.Configure(config.Default(), ModeFresh); err == nil {
		t.Error("no satellites enabled must be an error")
	}
}

func TestConfigureSectionsRoundTrip(t *testing.T) {
	answers := map[string]string{
		"station.name": "pi", "sdr.type": "airspy_mini", "satellites.enabled": "METEOR-M2 4",
		"web.admin.enabled":             "false",
		"section.notifications":         "true",
		"notifications.webhook.enabled": "true", "notifications.webhook.url": "https://ha.local/hook", "notifications.webhook.auth_token": "t",
		"notifications.discord.enabled": "true", "notifications.discord.noaa_webhook_url": "https://discord/x", "notifications.discord.meteor_webhook_url": "",
		"notifications.telegram.enabled": "false", "notifications.pushover.enabled": "false", "notifications.email.enabled": "false",
		"notifications.quality_gate.enabled": "true", "notifications.quality_gate.min_elevation": "30", "notifications.quality_gate.min_snr": "6",
		"section.daily": "true", "daily.best_of_day_push": "true", "daily.push_time": "21:00", "daily.timelapse.enabled": "true",
		"daily.mosaic.enabled": "false", "retention.delete_noaa_audio": "true", "retention.delete_meteor_audio": "false",
		"retention.delete_audio_older_than_days": "5", "retention.prune_images_older_than_days": "90", "community.contribute_composites": "true",
		"section.panel": "false", "section.advanced": "true",
		"sdr.use_device_string":       "false",
		"satellites.meteor-m2_4.gain": "21", "satellites.meteor-m2_4.bias_tee": "true", "satellites.meteor-m2_4.min_elevation": "25",
		"satellites.meteor-m2_4.sun_min_elevation": "6", "satellites.meteor-m2_4.schedule_sun_gate": "false", "satellites.meteor-m2_4.interleaving_80k": "true",
		"scheduling.days_ahead": "5", "scheduling.resolve_overlaps": "true", "scheduling.prefer_meteor_over_noaa": "false", "scheduling.tle_refresh_hour_utc": "3",
		"capture.noaa_memory_threshold_mb": "0", "capture.meteor_memory_threshold_mb": "100",
		"processing.noaa.day_enhancements": "MSA MCIR", "processing.noaa.night_enhancements": "MCIR", "processing.noaa.map_country_borders": "false",
		"processing.noaa.crop_telemetry_wedges": "true", "processing.noaa.jpg_quality": "80",
		"processing.meteor.day_enhancements": "221 321", "processing.meteor.night_enhancements": "654", "processing.meteor.flip_northbound": "false",
		"processing.meteor.draw_map_overlay": "true", "processing.meteor.equidistant_projection": "false", "processing.meteor.jpg_quality": "85",
		"processing.polar_az_el": "false", "processing.polar_direction": "true",
		"watchdog.enabled": "true", "watchdog.max_hours_without_capture": "24", "watchdog.disk_usage_threshold_pct": "80", "log_level": "debug",
	}
	p, _ := scripted(answers)
	w := &Wizard{P: p, Probe: &Probe{Hostname: "pi"}}
	cfg, err := w.Configure(config.Default(), ModeFresh)
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Notifications
	if !n.Webhook.Enabled || n.Webhook.URL != "https://ha.local/hook" || n.Discord.MeteorWebhookURL != "https://discord/x" ||
		!n.QualityGate.Enabled || n.QualityGate.MinSNR != 6 {
		t.Errorf("notifications = %+v", n)
	}
	if !cfg.Daily.BestOfDayPush || cfg.Daily.PushTime != "21:00" || cfg.Retention.PruneImagesOlderThanDays != 90 || !cfg.Community.ContributeComposites {
		t.Errorf("daily/retention = %+v %+v", cfg.Daily, cfg.Retention)
	}
	var m4 config.Satellite
	for _, s := range cfg.Satellites {
		if s.Name == "METEOR-M2 4" {
			m4 = s
		}
	}
	if m4.Gain != 21 || !m4.BiasTee || m4.MinElevation != 25 || m4.ScheduleSunMinElevation != nil || !m4.Interleaving80k {
		t.Errorf("meteor m2 4 = %+v", m4)
	}
	if cfg.Scheduling.DaysAhead != 5 || cfg.Scheduling.TLERefreshHourUTC != 3 || cfg.Capture.MeteorMemoryThresholdMB != 100 {
		t.Errorf("scheduling/capture = %+v %+v", cfg.Scheduling, cfg.Capture)
	}
	if strings.Join(cfg.Processing.NOAA.DayEnhancements, " ") != "MSA MCIR" || cfg.Processing.Meteor.JPGQuality != 85 || cfg.Processing.PolarAzEl {
		t.Errorf("processing = %+v", cfg.Processing)
	}
	if cfg.Watchdog.DiskUsageThresholdPct != 80 || cfg.LogLevel != "debug" || cfg.Web.Listen != ":80" || cfg.Scheduling.DryRun {
		t.Errorf("watchdog/log/web = %+v %s", cfg.Watchdog, cfg.LogLevel)
	}

	// The rendered YAML must load back through the daemon's strict loader.
	text, err := RenderConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseConfig(string(text))
	if err != nil {
		t.Fatalf("rendered config does not load: %v\n%s", err, text)
	}
	if back.Notifications.Webhook.AuthToken != "t" || back.Daily.PushTime != "21:00" || len(back.Satellites) != 5 {
		t.Errorf("round trip lost data: %+v", back)
	}
	red := Redacted(text)
	if strings.Contains(red, "https://ha.local/hook") || strings.Contains(red, "auth_token: t") {
		t.Errorf("secrets not redacted:\n%s", red)
	}
	if !strings.Contains(red, "latitude:") {
		t.Error("redaction damaged the document")
	}
}

func TestParseProbe(t *testing.T) {
	out := `os=Debian GNU/Linux 13 (trixie)
arch=aarch64
hostname=raspinoaa
model=Raspberry Pi 4 Model B Rev 1.4
mem_mb=3794
tz=Europe/Copenhagen
sudo_nopasswd=yes
satdump=/usr/bin/satdump
satdump_version=SatDump v1.2.2
rtl_sdr=/usr/local/bin/rtl_sdr
rtlsdr_usb=2838
nginx_active=active
rnv3_active=inactive
rnv3_version=
rn2_home=/home/pi/raspberry-noaa-v2
rn2_db=yes
rn2_captures=1214
rn2_crons=5
at_jobs=12
`
	p := parseProbe(out)
	if p.OS != "Debian GNU/Linux 13 (trixie)" || p.Arch != "aarch64" || p.MemMB != 3794 || !p.SudoNoPass || p.RTLSDRUSB != "2838" ||
		!p.NginxActive || p.RNV3Active || p.RNV3Version != "" || p.RN2Home != "/home/pi/raspberry-noaa-v2" || !p.RN2DB ||
		p.RN2Captures != 1214 || p.RN2Crons != 5 || p.AtJobs != 12 {
		t.Errorf("probe = %+v", p)
	}
	s := p.Summary()
	for _, want := range []string{"SatDump v1.2.2", "0bda:2838", "1214 captures", "rnv3: not installed", "passwordless sudo: yes"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	if b := parseProbe("arch=aarch64\nsudo_nopasswd=no\n"); b.SudoNoPass || b.RN2Home != "" || !strings.Contains(b.Summary(), "will be built") {
		t.Errorf("bare probe = %+v", b)
	}
}

func TestPrefillFromRN2(t *testing.T) {
	settings := `
latitude: 55.6761
longitude: 12.5683
altitude: 24.0
receiver_type: "airspy_mini"
ground_station_location: "Copenhagen, DK"
web_server_name: raspinoaa.local
noaa_15_schedule: false
noaa_19_schedule: true
noaa_19_gain: 21.5
noaa_19_sat_min_elevation: 25
meteor_m2_3_schedule: true
meteor_m2_3_gain: 18
meteor_m2_3_schedule_sun_min_elevation: -3
meteor_m2_3_80k_interleaving: true
meteor_m2_4_schedule: false
days_to_schedule_passes: 3
delete_noaa_audio: true
delete_files_older_than_days: 7
noaa_daytime_enhancements: "MSA MCIR HVC"
flip_meteor_image: false
enable_telegram_push: true
telegram_bot_token: "123:abc"
telegram_chat_id: "42"
enable_discord_push: false
pushover_link_url: "https://sat.example.org/captures/listImages"
enable_push_quality_gate: true
push_min_max_elevation: 35
lock_admin_page: true
admin_username: "ops"
admin_password: "letmein"
web_passes_date_format: "d.m.Y"
web_datetime_format: "d.m.Y H:i:s"
log_level: DEBUG
enable_coronal: true
watchdog_max_hours_without_capture: 12
contribute_to_community_composites: true
`
	cfg := config.Default()
	if err := PrefillFromRN2(cfg, settings); err != nil {
		t.Fatal(err)
	}
	if cfg.Station.Latitude != 55.6761 || cfg.Station.Location != "Copenhagen, DK" || cfg.Station.Name != "raspinoaa.local" || cfg.SDR.Type != "airspy_mini" {
		t.Errorf("station/sdr = %+v %+v", cfg.Station, cfg.SDR)
	}
	sat := map[string]config.Satellite{}
	for _, s := range cfg.Satellites {
		sat[s.Name] = s
	}
	if !sat["NOAA 19"].Enabled || sat["NOAA 19"].Gain != 21.5 || sat["NOAA 19"].MinElevation != 25 || sat["NOAA 15"].Enabled {
		t.Errorf("noaa = %+v", sat["NOAA 19"])
	}
	if m := sat["METEOR-M2 3"]; !m.Enabled || m.Gain != 18 || m.ScheduleSunMinElevation == nil || *m.ScheduleSunMinElevation != -3 || !m.Interleaving80k {
		t.Errorf("meteor = %+v", m)
	}
	if sat["METEOR-M2 4"].Enabled {
		t.Error("meteor m2 4 should be disabled")
	}
	if cfg.Scheduling.DaysAhead != 3 || !cfg.Retention.DeleteNOAAAudio || cfg.Retention.AudioOlderThanDays != 7 {
		t.Errorf("scheduling/retention = %+v %+v", cfg.Scheduling, cfg.Retention)
	}
	if strings.Join(cfg.Processing.NOAA.DayEnhancements, " ") != "MSA MCIR HVC" || cfg.Processing.Meteor.FlipNorthbound {
		t.Errorf("processing = %+v", cfg.Processing)
	}
	n := cfg.Notifications
	if !n.Telegram.Enabled || n.Telegram.BotToken != "123:abc" || n.Telegram.ChatID != "42" || n.Discord.Enabled ||
		n.Pushover.LinkURL != "https://sat.example.org" || !n.QualityGate.Enabled || n.QualityGate.MinElevation != 35 {
		t.Errorf("notifications = %+v", n)
	}
	if !cfg.Web.Admin.Enabled || cfg.Web.Admin.Username != "ops" || bcrypt.CompareHashAndPassword([]byte(cfg.Web.Admin.PasswordHash), []byte("letmein")) != nil {
		t.Errorf("admin = %+v", cfg.Web.Admin)
	}
	if cfg.Web.DateFormat != "02.01.2006" || cfg.Web.DateTimeFormat != "02.01.2006 15:04:05" || cfg.LogLevel != "debug" || !cfg.Web.Instruments.Coronal {
		t.Errorf("web = %+v log=%s", cfg.Web, cfg.LogLevel)
	}
	if cfg.Watchdog.MaxHoursWithoutCapture != 12 || !cfg.Community.ContributeComposites {
		t.Errorf("watchdog/community = %+v %+v", cfg.Watchdog, cfg.Community)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("prefilled config invalid: %v", err)
	}
	if err := PrefillFromRN2(config.Default(), "not: [valid"); err == nil {
		t.Error("broken yaml accepted")
	}
	// The placeholder pushover link is not carried over.
	cfg2 := config.Default()
	PrefillFromRN2(cfg2, `pushover_link_url: "https://<url-to-webpanel>/captures/listImages"`)
	if cfg2.Notifications.Pushover.LinkURL != "" {
		t.Error("placeholder link carried over")
	}
}

func TestPanelURL(t *testing.T) {
	cases := map[[2]string]string{
		{"raspinoaa", ":80"}:        "http://raspinoaa/",
		{"raspinoaa", ":8080"}:      "http://raspinoaa:8080/",
		{"raspinoaa:2222", ":8080"}: "http://raspinoaa:8080/",
		{"10.0.0.5", "0.0.0.0:80"}:  "http://10.0.0.5/",
	}
	for in, want := range cases {
		if got := PanelURL(in[0], in[1]); got != want {
			t.Errorf("PanelURL(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestCannedInvalidAnswersFallThroughOrFail(t *testing.T) {
	// Invalid canned bool: reported, dropped, then read interactively.
	p, out := scripted(map[string]string{"apply": "flase", "n": "lots", "sat": "NOAA 99"}, "y", "7")
	if !p.AskBool("apply", "Apply", false) {
		t.Error("interactive fallback not used")
	}
	if p.AskInt("n", "N", 1) != 7 {
		t.Error("int fallback not used")
	}
	if _, ok := p.Answers["apply"]; ok {
		t.Error("invalid canned answer not dropped")
	}
	if !strings.Contains(out.String(), `apply = "flase" is invalid`) {
		t.Errorf("no report of the invalid canned value:\n%s", out.String())
	}
	// Input exhausted: the run fails instead of spinning.
	var failed error
	p.Fatal = func(err error) { failed = err }
	sel := p.AskMulti("sat", "S", []string{"NOAA 19"}, map[string]bool{"NOAA 19": true})
	if !sel["NOAA 19"] {
		t.Error("multi with unknown canned option should keep the current selection at EOF")
	}
	if failed == nil {
		t.Error("EOF after an invalid canned value must fail the run")
	}
	failed = nil
	p2, _ := scripted(map[string]string{"x": "nope"})
	p2.Fatal = func(err error) { failed = err }
	p2.AskFloat("x", "X", 1)
	if failed == nil || !strings.Contains(failed.Error(), "x") {
		t.Errorf("invalid float with no input must fail: %v", failed)
	}
}

func TestRedactedMasksEveryCredentialKey(t *testing.T) {
	text := "station:\n  name: pi\nnotifications:\n  webhook:\n    url: https://ha/hook\n    auth_token: tok\n  discord:\n    noaa_webhook_url: https://d/1\n    meteor_webhook_url: https://d/2\n  telegram:\n    bot_token: 123:abc\n    chat_id: \"42\"\n  pushover:\n    link_url: \"\"\n  email:\n    smtp_password: pw\nweb:\n  admin:\n    password_hash: $2a$10$x\n"
	red := Redacted([]byte(text))
	for _, leak := range []string{"https://ha/hook", "SECRETTOK", "https://d/1", "https://d/2", "123:abc", "smtp_password: pw", "$2a$10$x"} {
		if strings.Contains(red, leak) {
			t.Errorf("leaked %q:\n%s", leak, red)
		}
	}
	for _, keep := range []string{"name: pi", "chat_id: \"42\"", `link_url: ""`} {
		if !strings.Contains(red, keep) {
			t.Errorf("over-redacted, missing %q:\n%s", keep, red)
		}
	}
	for k, want := range map[string]bool{"notifications.discord.noaa_webhook_url": true, "web.admin.password": true, "pi.password": true,
		"notifications.pushover.user_key": false, "station.name": false, "notifications.telegram.chat_id": false} {
		if IsSecretKey(k) != want {
			t.Errorf("IsSecretKey(%q) = %v", k, !want)
		}
	}
}

func TestSecretsNotEchoedInReconfigure(t *testing.T) {
	base := config.Default()
	base.Notifications.Telegram = config.Telegram{Enabled: true, BotToken: "123:SECRET", ChatID: "42"}
	base.Notifications.Discord = config.Discord{Enabled: true, NOAAWebhookURL: "https://discord/SECRET"}
	answers := map[string]string{"station.name": "pi", "sdr.type": "rtlsdr", "satellites.enabled": "METEOR-M2 3", "web.admin.enabled": "false",
		"section.notifications": "true", "notifications.webhook.enabled": "false", "notifications.discord.enabled": "true",
		"notifications.telegram.enabled": "true", "notifications.pushover.enabled": "false", "notifications.email.enabled": "false",
		"notifications.quality_gate.enabled": "false", "section.daily": "false", "section.panel": "false", "section.advanced": "false"}
	p, out := scripted(answers, "", "", "", "") // Enter keeps every secret
	w := &Wizard{P: p, Probe: &Probe{Hostname: "pi"}}
	cfg, err := w.Configure(base, ModeReconfigure)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Errorf("credential echoed to the terminal:\n%s", out.String())
	}
	if cfg.Notifications.Telegram.BotToken != "123:SECRET" || cfg.Notifications.Discord.NOAAWebhookURL != "https://discord/SECRET" ||
		cfg.Notifications.Discord.MeteorWebhookURL != "https://discord/SECRET" {
		t.Errorf("secrets not preserved on Enter: %+v", cfg.Notifications)
	}
}

func TestReconfigureOffersPortMoveWhenNginxGone(t *testing.T) {
	base := config.Default()
	base.Web.Listen = ":8080"
	answers := map[string]string{"station.name": "pi", "sdr.type": "rtlsdr", "satellites.enabled": "METEOR-M2 3", "web.admin.enabled": "false",
		"section.notifications": "false", "section.daily": "false", "section.panel": "false", "section.advanced": "false"}
	p, _ := scripted(answers) // Enter → default yes
	w := &Wizard{P: p, Probe: &Probe{Hostname: "pi", NginxActive: false}}
	cfg, err := w.Configure(base, ModeReconfigure)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Listen != ":80" {
		t.Errorf("listen = %s, want :80", cfg.Web.Listen)
	}
	// With nginx still active the question is not asked and :8080 stays.
	p, out := scripted(answers)
	w = &Wizard{P: p, Probe: &Probe{Hostname: "pi", NginxActive: true}}
	cfg, _ = w.Configure(base, ModeReconfigure)
	if cfg.Web.Listen != ":8080" || strings.Contains(out.String(), "move it to the standard port") {
		t.Errorf("port move offered while nginx holds :80 (listen=%s)", cfg.Web.Listen)
	}
}

func TestPublishSection(t *testing.T) {
	answers := map[string]string{"station.name": "pi", "sdr.type": "rtlsdr", "satellites.enabled": "METEOR-M2 3", "web.admin.enabled": "false",
		"section.notifications": "true", "notifications.webhook.enabled": "false", "notifications.discord.enabled": "false",
		"notifications.telegram.enabled": "false", "notifications.pushover.enabled": "false", "notifications.email.enabled": "false",
		"publish.enabled": "true", "publish.endpoint.1.name": "permi", "publish.endpoint.1.url": "https://permi.dk/api/station/webhook",
		"publish.endpoint.1.token": "SECRET", "publish.endpoint.1.images": "true", "publish.endpoint.1.another": "false", "publish.backfill_days": "14",
		"section.daily": "false", "section.panel": "false", "section.advanced": "false"}
	p, out := scripted(answers)
	w := &Wizard{P: p, Probe: &Probe{Hostname: "pi"}}
	cfg, err := w.Configure(config.Default(), ModeFresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Publish.Endpoints) != 1 || cfg.Publish.Endpoints[0].Token != "SECRET" || cfg.Publish.Endpoints[0].URL != "https://permi.dk/api/station/webhook" ||
		!cfg.Publish.Endpoints[0].Images || cfg.Publish.BackfillDays != 14 {
		t.Errorf("publish = %+v", cfg.Publish)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Error("webhook secret echoed")
	}
	text, _ := RenderConfig(cfg)
	if !strings.Contains(Redacted(text), "token: ********") {
		t.Error("token not redacted in the preview")
	}
	back, err := ParseConfig(string(text))
	if err != nil || len(back.Publish.Endpoints) != 1 {
		t.Errorf("round trip: %v %+v", err, back.Publish)
	}
	// Reconfigure: an existing endpoint can be dropped.
	answers["publish.endpoint.1.keep"] = "false"
	answers["publish.endpoint.2.name"] = "other"
	answers["publish.endpoint.2.url"] = "https://other.example/hook"
	answers["publish.endpoint.2.token"] = "T2"
	answers["publish.endpoint.2.images"] = "false"
	answers["publish.endpoint.2.another"] = "false"
	p, _ = scripted(answers)
	w = &Wizard{P: p, Probe: &Probe{Hostname: "pi"}}
	cfg2, err := w.Configure(cfg, ModeReconfigure)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg2.Publish.Endpoints) != 1 || cfg2.Publish.Endpoints[0].Name != "other" || cfg2.Publish.Endpoints[0].Images {
		t.Errorf("reconfigured publish = %+v", cfg2.Publish)
	}
}
