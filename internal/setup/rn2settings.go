package setup

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/perhp/rnv3/internal/config"
)

// rn2Settings is RN2's settings.yml as a loose map.
type rn2Settings map[string]any

func parseRN2Settings(text string) (rn2Settings, error) {
	s := rn2Settings{}
	if err := yaml.Unmarshal([]byte(text), &s); err != nil {
		return nil, fmt.Errorf("settings.yml: %w", err)
	}
	return s, nil
}

func (s rn2Settings) str(key string) (string, bool) {
	v, ok := s[key]
	if !ok || v == nil {
		return "", false
	}
	return strings.TrimSpace(fmt.Sprint(v)), true
}

func (s rn2Settings) float(key string) (float64, bool) {
	switch v := s[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		var f float64
		if _, err := fmt.Sscan(v, &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func (s rn2Settings) int(key string) (int, bool) {
	f, ok := s.float(key)
	return int(f), ok
}

func (s rn2Settings) boolean(key string) (bool, bool) {
	switch v := s[key].(type) {
	case bool:
		return v, true
	case string:
		return strings.EqualFold(v, "true"), true
	}
	return false, false
}

// rn2Satellites maps RN2's per-satellite key prefixes onto rnv3 names.
var rn2Satellites = map[string]string{
	"NOAA 15": "noaa_15", "NOAA 18": "noaa_18", "NOAA 19": "noaa_19",
	"METEOR-M2 3": "meteor_m2_3", "METEOR-M2 4": "meteor_m2_4",
}

// PrefillFromRN2 overlays RN2's settings onto cfg: station, SDR, per-
// satellite settings, processing, retention, panel, notifications,
// watchdog, community. Only keys present in settings.yml are applied, so
// rnv3's defaults stand for anything RN2 never had.
func PrefillFromRN2(cfg *config.Config, text string) error {
	s, err := parseRN2Settings(text)
	if err != nil {
		return err
	}
	setF := func(dst *float64, key string) {
		if v, ok := s.float(key); ok {
			*dst = v
		}
	}
	setI := func(dst *int, key string) {
		if v, ok := s.int(key); ok {
			*dst = v
		}
	}
	setB := func(dst *bool, key string) {
		if v, ok := s.boolean(key); ok {
			*dst = v
		}
	}
	setS := func(dst *string, key string) {
		if v, ok := s.str(key); ok {
			*dst = v
		}
	}
	setList := func(dst *[]string, key string) {
		if v, ok := s.str(key); ok && v != "" {
			*dst = strings.Fields(v)
		}
	}

	setF(&cfg.Station.Latitude, "latitude")
	setF(&cfg.Station.Longitude, "longitude")
	setF(&cfg.Station.Altitude, "altitude")
	setS(&cfg.Station.Location, "ground_station_location")
	setS(&cfg.Station.Name, "web_server_name")
	if v, ok := s.str("receiver_type"); ok {
		if _, known := config.ReceiverTypes[v]; known {
			cfg.SDR.Type = v
		}
	}
	setB(&cfg.SDR.UseDeviceString, "use_device_string")

	for i := range cfg.Satellites {
		sat := &cfg.Satellites[i]
		prefix, ok := rn2Satellites[sat.Name]
		if !ok {
			continue
		}
		setB(&sat.Enabled, prefix+"_schedule")
		setS(&sat.DeviceID, prefix+"_sdr_device_id")
		setF(&sat.FreqOffset, prefix+"_freq_offset")
		setB(&sat.BiasTee, prefix+"_enable_bias_tee")
		setF(&sat.Gain, prefix+"_gain")
		setF(&sat.SunMinElevation, prefix+"_sun_min_elevation")
		setF(&sat.MinElevation, prefix+"_sat_min_elevation")
		if v, ok := s.float(prefix + "_schedule_sun_min_elevation"); ok {
			sat.ScheduleSunMinElevation = &v
		}
		setB(&sat.Interleaving80k, prefix+"_80k_interleaving")
	}

	setI(&cfg.Scheduling.DaysAhead, "days_to_schedule_passes")
	setB(&cfg.Scheduling.ResolveOverlaps, "select_best_overlapping_passes")
	setB(&cfg.Scheduling.PreferMeteorOverNOAA, "select_meteor_pass_over_noaa")
	setI(&cfg.Capture.NOAAMemoryThresholdMB, "noaa_memory_threshold")
	setI(&cfg.Capture.MeteorMemoryThresholdMB, "meteor_m2_memory_threshold")

	setList(&cfg.Processing.NOAA.DayEnhancements, "noaa_daytime_enhancements")
	setList(&cfg.Processing.NOAA.NightEnhancements, "noaa_nighttime_enhancements")
	setI(&cfg.Processing.NOAA.JPGQuality, "noaa_jpg_image_quality")
	setB(&cfg.Processing.NOAA.CropWedges, "noaa_crop_toptobottom")
	setB(&cfg.Processing.NOAA.MapCountryBorders, "noaa_map_country_border_enable")
	setList(&cfg.Processing.Meteor.DayEnhancements, "meteor_daytime_enhancements")
	setList(&cfg.Processing.Meteor.NightEnhancements, "meteor_nighttime_enhancements")
	setI(&cfg.Processing.Meteor.JPGQuality, "meteor_jpg_image_quality")
	setB(&cfg.Processing.Meteor.FlipNorthbound, "flip_meteor_image")
	setB(&cfg.Processing.Meteor.DrawMapOverlay, "meteor_draw_map_overlay")
	setB(&cfg.Processing.Meteor.EquidistantProjection, "meteor_create_equidistant_projection")
	setB(&cfg.Processing.PolarAzEl, "produce_polar_az_el_graph")
	setB(&cfg.Processing.PolarDirect, "produce_polar_direction_graph")

	setB(&cfg.Retention.DeleteNOAAAudio, "delete_noaa_audio")
	setB(&cfg.Retention.DeleteMeteorAudio, "delete_meteor_audio")
	setI(&cfg.Retention.AudioOlderThanDays, "delete_files_older_than_days")
	// RN2's delete_older_than_n was never wired to anything; rnv3 keeps
	// images forever unless the operator opts in, so it is not carried over.

	setB(&cfg.Daily.BestOfDayPush, "enable_best_of_day_push")
	setB(&cfg.Daily.Timelapse.Enabled, "enable_daily_timelapse")
	setList(&cfg.Daily.Timelapse.Suffixes, "daily_timelapse_suffixes")
	setB(&cfg.Daily.Mosaic.Enabled, "enable_daily_mosaic")
	setList(&cfg.Daily.Mosaic.Suffixes, "daily_mosaic_suffixes")
	setF(&cfg.Daily.Mosaic.MinSNR, "daily_mosaic_min_snr")
	setB(&cfg.Daily.Mosaic.DaylightOnly, "daily_mosaic_daylight_only")

	n := &cfg.Notifications
	setB(&n.QualityGate.Enabled, "enable_push_quality_gate")
	setF(&n.QualityGate.MinElevation, "push_min_max_elevation")
	setF(&n.QualityGate.MinSNR, "push_min_snr")
	setB(&n.Webhook.Enabled, "enable_webhook_push")
	setS(&n.Webhook.URL, "webhook_push_url")
	setS(&n.Webhook.AuthToken, "webhook_push_auth_token")
	setB(&n.Discord.Enabled, "enable_discord_push")
	setS(&n.Discord.NOAAWebhookURL, "discord_noaa_webhook_url")
	setS(&n.Discord.MeteorWebhookURL, "discord_meteor_webhook_url")
	setB(&n.Telegram.Enabled, "enable_telegram_push")
	setS(&n.Telegram.BotToken, "telegram_bot_token")
	setS(&n.Telegram.ChatID, "telegram_chat_id")
	setB(&n.Pushover.Enabled, "enable_pushover_push")
	setS(&n.Pushover.APIToken, "pushover_apitoken")
	setS(&n.Pushover.User, "pushover_user")
	setI(&n.Pushover.Priority, "pushover_prio")
	if v, ok := s.str("pushover_link_url"); ok && !strings.Contains(v, "<") {
		n.Pushover.LinkURL = strings.TrimSuffix(v, "/captures/listImages")
	}
	setB(&n.Email.Enabled, "enable_email_push")
	if v, ok := s.str("email_push_address"); ok && v != "test@ifttt.com" {
		n.Email.To = v
	}

	setB(&cfg.Watchdog.Enabled, "enable_health_watchdog")
	setI(&cfg.Watchdog.MaxHoursWithoutCapture, "watchdog_max_hours_without_capture")
	setI(&cfg.Watchdog.DiskUsageThresholdPct, "watchdog_disk_usage_threshold")
	setB(&cfg.Community.ContributeComposites, "contribute_to_community_composites")
	setS(&cfg.Community.URL, "contribute_to_community_composites_url")

	setB(&cfg.Web.Instruments.Satvis, "enable_satvis")
	setB(&cfg.Web.Instruments.Coronal, "enable_coronal")
	setB(&cfg.Web.Instruments.SolarTerminator, "enable_solar_terminator")
	setB(&cfg.Web.Admin.Enabled, "lock_admin_page")
	setS(&cfg.Web.Admin.Username, "admin_username")
	if pw, ok := s.str("admin_password"); ok && pw != "" && cfg.Web.Admin.PasswordHash == "" {
		// RN2 kept the admin password in clear text; carry it over hashed so
		// the lock keeps working with the credentials the operator knows.
		if h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost); err == nil {
			cfg.Web.Admin.PasswordHash = string(h)
		}
	}
	if v, ok := s.str("web_passes_date_format"); ok {
		cfg.Web.DateFormat = phpDateToGo(v)
	}
	if v, ok := s.str("web_datetime_format"); ok {
		cfg.Web.DateTimeFormat = phpDateToGo(v)
	}
	if v, ok := s.str("log_level"); ok {
		switch strings.ToLower(v) {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = strings.ToLower(v)
		}
	}
	return nil
}

// phpDateToGo converts the PHP date() format RN2's panel used into a Go
// layout; unknown characters pass through.
func phpDateToGo(php string) string {
	repl := map[rune]string{
		'd': "02", 'j': "2", 'm': "01", 'n': "1", 'Y': "2006", 'y': "06",
		'H': "15", 'G': "15", 'i': "04", 's': "05", 'D': "Mon", 'l': "Monday",
		'M': "Jan", 'F': "January", 'a': "pm", 'A': "PM", 'g': "3", 'h': "03",
	}
	var b strings.Builder
	for _, r := range php {
		if g, ok := repl[r]; ok {
			b.WriteString(g)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
