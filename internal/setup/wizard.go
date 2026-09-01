package setup

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/perhp/rnv3/internal/config"
)

// Mode is what the operator wants done.
type Mode string

const (
	ModeSideBySide  Mode = "side-by-side" // install next to a running RN2: dry-run, panel on :8080
	ModeFresh       Mode = "fresh"        // rnv3 is the station
	ModeCutover     Mode = "cutover"      // retire RN2, hand the SDR to rnv3
	ModeReconfigure Mode = "reconfigure"  // edit an existing install's config
)

// Wizard turns prompts into a config.Config.
type Wizard struct {
	P     *Prompter
	Probe *Probe
}

// ChooseMode presents the modes that make sense for this Pi.
func (w *Wizard) ChooseMode() Mode {
	type opt struct {
		label string
		mode  Mode
	}
	var opts []opt
	def := ""
	if w.Probe.RN2Home != "" {
		opts = append(opts, opt{"Install rnv3 side by side with raspberry-noaa-v2 (dry-run, panel on :8080) — validate first", ModeSideBySide})
		def = opts[0].label
	}
	opts = append(opts, opt{"Install rnv3 as the station (no RN2, or RN2 already retired)", ModeFresh})
	if w.Probe.RN2Home != "" && w.Probe.RNV3Version != "" {
		opts = append(opts, opt{"Cut over from raspberry-noaa-v2 to rnv3 (rnv3 validated side by side)", ModeCutover})
	}
	if w.Probe.RNV3Config != "" {
		opts = append(opts, opt{"Reconfigure the existing rnv3 install", ModeReconfigure})
		if def == "" {
			def = opts[len(opts)-1].label
		}
	}
	if def == "" {
		def = opts[0].label
	}
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i] = o.label
	}
	// Answers may name the mode directly.
	if v, ok := w.P.Answers["mode"]; ok {
		for _, o := range opts {
			if string(o.mode) == v {
				w.P.Say("mode: %s  (from answers)", v)
				return o.mode
			}
		}
	}
	chosen := w.P.AskChoice("mode", "What do you want to do?", labels, def)
	for _, o := range opts {
		if o.label == chosen {
			w.P.record("mode", string(o.mode))
			return o.mode
		}
	}
	return ModeFresh
}

// Configure asks the essentials, then the optional sections, starting from
// base (defaults, RN2 prefill, or the existing config).
func (w *Wizard) Configure(base *config.Config, mode Mode) (*config.Config, error) {
	cfg := *base // shallow copy; slices are replaced, not mutated in place
	cfg.Satellites = append([]config.Satellite(nil), base.Satellites...)
	p := w.P

	p.Say("")
	p.Say("── Station ──")
	def := cfg.Station.Name
	if def == "" || def == "raspberry-noaa.localdomain" {
		def = w.Probe.Hostname
	}
	cfg.Station.Name = p.AskRequired("station.name", "Station hostname (used in links)", def)
	cfg.Station.Location = p.Ask("station.location", "Location shown in notifications (blank = none)", cfg.Station.Location)
	cfg.Station.Latitude = p.AskFloat("station.latitude", "Latitude (decimal degrees, north positive)", cfg.Station.Latitude)
	cfg.Station.Longitude = p.AskFloat("station.longitude", "Longitude (decimal degrees, east positive)", cfg.Station.Longitude)
	cfg.Station.Altitude = p.AskFloat("station.altitude", "Altitude (metres)", cfg.Station.Altitude)

	p.Say("")
	p.Say("── SDR ──")
	types := make([]string, 0, len(config.ReceiverTypes))
	for k := range config.ReceiverTypes {
		types = append(types, k)
	}
	sort.Strings(types)
	cfg.SDR.Type = p.AskChoice("sdr.type", "Receiver", types, cfg.SDR.Type)

	p.Say("")
	p.Say("── Satellites ──")
	names := make([]string, len(cfg.Satellites))
	enabled := map[string]bool{}
	for i, s := range cfg.Satellites {
		names[i] = s.Name
		enabled[s.Name] = s.Enabled
	}
	enabled = p.AskMulti("satellites.enabled", "Satellites to capture", names, enabled)
	any := false
	for i := range cfg.Satellites {
		cfg.Satellites[i].Enabled = enabled[cfg.Satellites[i].Name]
		any = any || cfg.Satellites[i].Enabled
	}
	if !any {
		return nil, fmt.Errorf("no satellite enabled — nothing would ever be captured")
	}

	p.Say("")
	p.Say("── Panel admin ──")
	cfg.Web.Admin.Enabled = p.AskBool("web.admin.enabled", "Require a login for the admin pages", cfg.Web.Admin.Enabled)
	if cfg.Web.Admin.Enabled {
		cfg.Web.Admin.Username = p.AskRequired("web.admin.username", "Admin username", cfg.Web.Admin.Username)
		for {
			pw := p.AskSecret("web.admin.password", "Admin password", cfg.Web.Admin.PasswordHash != "")
			if pw == "" && cfg.Web.Admin.PasswordHash != "" {
				break
			}
			if len(pw) < 4 {
				p.Say("  at least 4 characters, please")
				if p.Exhausted() {
					return nil, fmt.Errorf("web.admin.password is required when the admin lock is enabled")
				}
				continue
			}
			h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
			if err != nil {
				return nil, err
			}
			cfg.Web.Admin.PasswordHash = string(h)
			break
		}
	}

	switch mode {
	case ModeSideBySide:
		cfg.Scheduling.DryRun = true
		if w.Probe.NginxActive || strings.TrimSpace(cfg.Web.Listen) == ":80" {
			cfg.Web.Listen = ":8080"
		}
		p.Say("")
		p.Say("Side-by-side: scheduling.dry_run = true (RN2 keeps the SDR), panel on %s.", cfg.Web.Listen)
	case ModeFresh:
		cfg.Scheduling.DryRun = false
		if cfg.Web.Listen == "" || cfg.Web.Listen == ":8080" {
			cfg.Web.Listen = ":80"
		}
	case ModeReconfigure:
		// A panel left on :8080 after RN2 is gone is the side-by-side
		// setting outliving its reason.
		if cfg.Web.Listen == ":8080" && !w.Probe.NginxActive {
			p.Say("")
			if p.AskBool("web.move_to_80", "The panel is on :8080 and nginx is not running — move it to the standard port 80", true) {
				cfg.Web.Listen = ":80"
			}
		}
	}

	p.Say("")
	if p.AskBool("section.notifications", "Configure notifications (webhook, Discord, Telegram, Pushover, email)?", anyNotification(&cfg)) {
		w.notifications(&cfg)
	}
	if p.AskBool("section.daily", "Configure daily summary & retention?", false) {
		w.daily(&cfg)
	}
	if p.AskBool("section.panel", "Configure panel instruments & TLS?", false) {
		w.panel(&cfg)
	}
	if p.AskBool("section.advanced", "Configure advanced SDR & processing settings?", false) {
		w.advanced(&cfg)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func anyNotification(c *config.Config) bool {
	n := c.Notifications
	return n.Webhook.Enabled || n.Discord.Enabled || n.Telegram.Enabled || n.Pushover.Enabled || n.Email.Enabled
}

// secret asks for a credential without echoing it; empty input keeps the
// current value (shown as [unchanged] when there is one).
func (w *Wizard) secret(key, label, current string) string {
	if v := w.P.AskSecret(key, label, current != ""); v != "" {
		return v
	}
	return current
}

// secretRequired is secret, repeated until a value exists.
func (w *Wizard) secretRequired(key, label, current string) string {
	for {
		if v := w.secret(key, label, current); v != "" {
			return v
		}
		w.P.Say("  a value is required")
		if w.P.Exhausted() {
			w.P.fail(fmt.Errorf("no value for %s", key))
		}
	}
}

func (w *Wizard) notifications(cfg *config.Config) {
	p := w.P
	n := &cfg.Notifications
	p.Say("")
	p.Say("── Notifications ──")
	if n.Webhook.Enabled = p.AskBool("notifications.webhook.enabled", "Generic JSON webhook (Home Assistant, n8n, ...)", n.Webhook.Enabled); n.Webhook.Enabled {
		n.Webhook.URL = w.secretRequired("notifications.webhook.url", "  Webhook URL", n.Webhook.URL)
		n.Webhook.AuthToken = w.secret("notifications.webhook.auth_token", "  Bearer token (blank = none)", n.Webhook.AuthToken)
	}
	if n.Discord.Enabled = p.AskBool("notifications.discord.enabled", "Discord", n.Discord.Enabled); n.Discord.Enabled {
		n.Discord.NOAAWebhookURL = w.secret("notifications.discord.noaa_webhook_url", "  NOAA channel webhook URL", n.Discord.NOAAWebhookURL)
		n.Discord.MeteorWebhookURL = w.secret("notifications.discord.meteor_webhook_url", "  Meteor channel webhook URL (blank = same as NOAA)", n.Discord.MeteorWebhookURL)
		if n.Discord.MeteorWebhookURL == "" {
			n.Discord.MeteorWebhookURL = n.Discord.NOAAWebhookURL
		}
	}
	if n.Telegram.Enabled = p.AskBool("notifications.telegram.enabled", "Telegram", n.Telegram.Enabled); n.Telegram.Enabled {
		n.Telegram.BotToken = w.secretRequired("notifications.telegram.bot_token", "  Bot token (from @BotFather)", n.Telegram.BotToken)
		n.Telegram.ChatID = p.AskRequired("notifications.telegram.chat_id", "  Chat id", n.Telegram.ChatID)
	}
	if n.Pushover.Enabled = p.AskBool("notifications.pushover.enabled", "Pushover", n.Pushover.Enabled); n.Pushover.Enabled {
		n.Pushover.APIToken = w.secretRequired("notifications.pushover.api_token", "  API token", n.Pushover.APIToken)
		n.Pushover.User = w.secretRequired("notifications.pushover.user_key", "  User key", n.Pushover.User)
		n.Pushover.Priority = p.AskInt("notifications.pushover.priority", "  Priority (-2..2)", n.Pushover.Priority)
		n.Pushover.LinkURL = p.Ask("notifications.pushover.link_url", "  Panel URL for the notification link (blank = http://"+cfg.Station.Name+")", n.Pushover.LinkURL)
	}
	if n.Email.Enabled = p.AskBool("notifications.email.enabled", "Email", n.Email.Enabled); n.Email.Enabled {
		n.Email.To = p.AskRequired("notifications.email.to", "  Send to", n.Email.To)
		n.Email.From = p.AskRequired("notifications.email.from", "  Send from", n.Email.From)
		n.Email.SMTPHost = p.AskRequired("notifications.email.smtp_host", "  SMTP host", n.Email.SMTPHost)
		n.Email.SMTPPort = p.AskInt("notifications.email.smtp_port", "  SMTP port (587 STARTTLS, 465 TLS)", n.Email.SMTPPort)
		n.Email.SMTPUser = p.Ask("notifications.email.smtp_user", "  SMTP user (blank = no auth)", n.Email.SMTPUser)
		if n.Email.SMTPUser != "" {
			if pw := p.AskSecret("notifications.email.smtp_password", "  SMTP password", n.Email.SMTPPassword != ""); pw != "" {
				n.Email.SMTPPassword = pw
			}
		}
	}
	if anyNotification(cfg) {
		if n.QualityGate.Enabled = p.AskBool("notifications.quality_gate.enabled", "Quality gate: skip social pushes for weak passes (webhook still fires)", n.QualityGate.Enabled); n.QualityGate.Enabled {
			n.QualityGate.MinElevation = p.AskFloat("notifications.quality_gate.min_elevation", "  Minimum max-elevation (°)", n.QualityGate.MinElevation)
			n.QualityGate.MinSNR = p.AskFloat("notifications.quality_gate.min_snr", "  Minimum peak SNR (dB, 0 = ignore)", n.QualityGate.MinSNR)
		}
	}
}

func (w *Wizard) daily(cfg *config.Config) {
	p := w.P
	p.Say("")
	p.Say("── Daily summary & retention ──")
	cfg.Daily.BestOfDayPush = p.AskBool("daily.best_of_day_push", "Push a best-of-day summary", cfg.Daily.BestOfDayPush)
	if cfg.Daily.BestOfDayPush {
		cfg.Daily.PushTime = p.Ask("daily.push_time", "  at local time (HH:MM)", cfg.Daily.PushTime)
	}
	cfg.Daily.Timelapse.Enabled = p.AskBool("daily.timelapse.enabled", "Build daily timelapse GIFs (Meteor projections)", cfg.Daily.Timelapse.Enabled)
	cfg.Daily.Mosaic.Enabled = p.AskBool("daily.mosaic.enabled", "Build daily mosaics (Meteor projections)", cfg.Daily.Mosaic.Enabled)
	if cfg.Daily.Mosaic.Enabled {
		cfg.Daily.Mosaic.MinSNR = p.AskFloat("daily.mosaic.min_snr", "  Leave passes below this peak SNR out (dB, 0 = keep all)", cfg.Daily.Mosaic.MinSNR)
		cfg.Daily.Mosaic.DaylightOnly = p.AskBool("daily.mosaic.daylight_only", "  Daylight passes only", cfg.Daily.Mosaic.DaylightOnly)
	}
	cfg.Retention.DeleteNOAAAudio = p.AskBool("retention.delete_noaa_audio", "Delete NOAA audio (wav) after decoding", cfg.Retention.DeleteNOAAAudio)
	cfg.Retention.DeleteMeteorAudio = p.AskBool("retention.delete_meteor_audio", "Delete Meteor recordings (cadu) after decoding", cfg.Retention.DeleteMeteorAudio)
	if !cfg.Retention.DeleteNOAAAudio || !cfg.Retention.DeleteMeteorAudio {
		cfg.Retention.AudioOlderThanDays = p.AskInt("retention.delete_audio_older_than_days", "  Keep retained audio for (days)", cfg.Retention.AudioOlderThanDays)
	}
	cfg.Retention.PruneImagesOlderThanDays = p.AskInt("retention.prune_images_older_than_days", "Delete captures older than (days, 0 = keep forever)", cfg.Retention.PruneImagesOlderThanDays)
	cfg.Community.ContributeComposites = p.AskBool("community.contribute_composites", "Contribute Meteor recordings to the community composites", cfg.Community.ContributeComposites)
}

func (w *Wizard) panel(cfg *config.Config) {
	p := w.P
	p.Say("")
	p.Say("── Panel ──")
	cfg.Web.Instruments.Satvis = p.AskBool("web.instruments.satvis", "Show the satvis.space orbit view", cfg.Web.Instruments.Satvis)
	cfg.Web.Instruments.SolarTerminator = p.AskBool("web.instruments.solar_terminator", "Show the solar terminator map", cfg.Web.Instruments.SolarTerminator)
	cfg.Web.Instruments.Coronal = p.AskBool("web.instruments.coronal", "Show the coronal mass ejection cams (high data usage)", cfg.Web.Instruments.Coronal)
	cfg.Web.CapturesPerPage = p.AskInt("web.captures_per_page", "Captures per gallery page", cfg.Web.CapturesPerPage)
	cfg.Web.DateFormat = p.Ask("web.date_format", "Date format (Go layout, e.g. 01/02/2006 or 2006-01-02)", cfg.Web.DateFormat)
	cfg.Web.DateTimeFormat = p.Ask("web.datetime_format", "Date-time format (Go layout)", cfg.Web.DateTimeFormat)
	cfg.Web.TLS.Enabled = p.AskBool("web.tls.enabled", "Serve HTTPS as well", cfg.Web.TLS.Enabled)
	if cfg.Web.TLS.Enabled {
		cfg.Web.TLS.Listen = p.Ask("web.tls.listen", "  HTTPS listen address", cfg.Web.TLS.Listen)
		cfg.Web.TLS.CertFile = p.AskRequired("web.tls.cert_file", "  Certificate file (PEM) on the Pi", cfg.Web.TLS.CertFile)
		cfg.Web.TLS.KeyFile = p.AskRequired("web.tls.key_file", "  Key file (PEM) on the Pi", cfg.Web.TLS.KeyFile)
	}
}

func (w *Wizard) advanced(cfg *config.Config) {
	p := w.P
	p.Say("")
	p.Say("── Advanced SDR ──")
	cfg.SDR.UseDeviceString = p.AskBool("sdr.use_device_string", "Multiple SDRs: select by device id per satellite", cfg.SDR.UseDeviceString)
	rx := config.ReceiverTypes[cfg.SDR.Type]
	for i := range cfg.Satellites {
		s := &cfg.Satellites[i]
		if !s.Enabled {
			continue
		}
		key := "satellites." + strings.ToLower(strings.ReplaceAll(s.Name, " ", "_"))
		p.Say("%s:", s.Name)
		if rx.GainFlag != "" {
			s.Gain = p.AskFloat(key+".gain", "  Gain (0 = automatic)", s.Gain)
		}
		s.BiasTee = p.AskBool(key+".bias_tee", "  Bias tee", s.BiasTee)
		if rx.SupportsPPM {
			s.FreqOffset = p.AskFloat(key+".freq_offset_ppm", "  Frequency offset (ppm)", s.FreqOffset)
		}
		if cfg.SDR.UseDeviceString {
			s.DeviceID = p.Ask(key+".sdr_device_id", "  SDR device id", s.DeviceID)
		}
		s.MinElevation = p.AskFloat(key+".min_elevation", "  Minimum max-elevation to schedule (°)", s.MinElevation)
		s.SunMinElevation = p.AskFloat(key+".sun_min_elevation", "  Sun elevation splitting day/night enhancements (°)", s.SunMinElevation)
		if s.Type == config.SatMeteorLRPT {
			gate := -6.0
			if s.ScheduleSunMinElevation != nil {
				gate = *s.ScheduleSunMinElevation
			}
			if p.AskBool(key+".schedule_sun_gate", "  Skip night passes (visible-light instrument)", s.ScheduleSunMinElevation != nil) {
				gate = p.AskFloat(key+".schedule_sun_min_elevation", "    Sun must be above (°, negative = twilight ok)", gate)
				s.ScheduleSunMinElevation = &gate
			} else {
				s.ScheduleSunMinElevation = nil
			}
			s.Interleaving80k = p.AskBool(key+".interleaving_80k", "  80k interleaved LRPT mode", s.Interleaving80k)
		}
	}

	p.Say("")
	p.Say("── Scheduling & capture ──")
	cfg.Scheduling.DaysAhead = p.AskInt("scheduling.days_ahead", "Plan passes this many days ahead", cfg.Scheduling.DaysAhead)
	cfg.Scheduling.ResolveOverlaps = p.AskBool("scheduling.resolve_overlaps", "Resolve overlapping passes (keep the better one)", cfg.Scheduling.ResolveOverlaps)
	if cfg.Scheduling.ResolveOverlaps {
		cfg.Scheduling.PreferMeteorOverNOAA = p.AskBool("scheduling.prefer_meteor_over_noaa", "  Prefer Meteor over NOAA on overlap", cfg.Scheduling.PreferMeteorOverNOAA)
	}
	cfg.Scheduling.TLERefreshHourUTC = p.AskInt("scheduling.tle_refresh_hour_utc", "Daily TLE refresh hour (UTC)", cfg.Scheduling.TLERefreshHourUTC)
	cfg.Capture.NOAAMemoryThresholdMB = p.AskInt("capture.noaa_memory_threshold_mb", "Buffer NOAA captures in RAM when this much is free (MB, 0 = never)", cfg.Capture.NOAAMemoryThresholdMB)
	cfg.Capture.MeteorMemoryThresholdMB = p.AskInt("capture.meteor_memory_threshold_mb", "Buffer Meteor captures in RAM when this much is free (MB, 0 = never)", cfg.Capture.MeteorMemoryThresholdMB)

	p.Say("")
	p.Say("── Processing ──")
	cfg.Processing.NOAA.DayEnhancements = strings.Fields(p.Ask("processing.noaa.day_enhancements", "NOAA daytime enhancements", strings.Join(cfg.Processing.NOAA.DayEnhancements, " ")))
	cfg.Processing.NOAA.NightEnhancements = strings.Fields(p.Ask("processing.noaa.night_enhancements", "NOAA night enhancements", strings.Join(cfg.Processing.NOAA.NightEnhancements, " ")))
	cfg.Processing.NOAA.MapCountryBorders = p.AskBool("processing.noaa.map_country_borders", "NOAA map overlay with country borders", cfg.Processing.NOAA.MapCountryBorders)
	cfg.Processing.NOAA.CropWedges = p.AskBool("processing.noaa.crop_telemetry_wedges", "Crop NOAA telemetry wedges", cfg.Processing.NOAA.CropWedges)
	cfg.Processing.NOAA.JPGQuality = p.AskInt("processing.noaa.jpg_quality", "NOAA JPEG quality", cfg.Processing.NOAA.JPGQuality)
	cfg.Processing.Meteor.DayEnhancements = strings.Fields(p.Ask("processing.meteor.day_enhancements", "Meteor daytime enhancements", strings.Join(cfg.Processing.Meteor.DayEnhancements, " ")))
	cfg.Processing.Meteor.NightEnhancements = strings.Fields(p.Ask("processing.meteor.night_enhancements", "Meteor night enhancements", strings.Join(cfg.Processing.Meteor.NightEnhancements, " ")))
	cfg.Processing.Meteor.FlipNorthbound = p.AskBool("processing.meteor.flip_northbound", "Flip northbound Meteor images", cfg.Processing.Meteor.FlipNorthbound)
	cfg.Processing.Meteor.DrawMapOverlay = p.AskBool("processing.meteor.draw_map_overlay", "Meteor map overlay", cfg.Processing.Meteor.DrawMapOverlay)
	cfg.Processing.Meteor.EquidistantProjection = p.AskBool("processing.meteor.equidistant_projection", "Meteor equidistant projection", cfg.Processing.Meteor.EquidistantProjection)
	cfg.Processing.Meteor.JPGQuality = p.AskInt("processing.meteor.jpg_quality", "Meteor JPEG quality", cfg.Processing.Meteor.JPGQuality)
	cfg.Processing.PolarAzEl = p.AskBool("processing.polar_az_el", "Polar az/el plot per pass", cfg.Processing.PolarAzEl)
	cfg.Processing.PolarDirect = p.AskBool("processing.polar_direction", "Polar direction plot per pass", cfg.Processing.PolarDirect)

	p.Say("")
	p.Say("── Watchdog & logging ──")
	cfg.Watchdog.Enabled = p.AskBool("watchdog.enabled", "Health watchdog alerts", cfg.Watchdog.Enabled)
	if cfg.Watchdog.Enabled {
		cfg.Watchdog.MaxHoursWithoutCapture = p.AskInt("watchdog.max_hours_without_capture", "  Alert after this many hours without a capture", cfg.Watchdog.MaxHoursWithoutCapture)
		cfg.Watchdog.DiskUsageThresholdPct = p.AskInt("watchdog.disk_usage_threshold_pct", "  Alert when image storage is this full (%)", cfg.Watchdog.DiskUsageThresholdPct)
	}
	cfg.LogLevel = p.AskChoice("log_level", "Log level", []string{"debug", "info", "warn", "error"}, strings.ToLower(cfg.LogLevel))
}
