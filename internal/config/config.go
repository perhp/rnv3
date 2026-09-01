// Package config defines the rnv3 configuration schema, defaults, loading,
// and validation. One YAML file replaces RN2's settings.yml + Ansible-rendered
// ~/.noaa-v2.conf + Config.php + nginx vhosts.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SatelliteType selects the SatDump pipeline family for a satellite.
type SatelliteType string

const (
	SatNOAAAPT    SatelliteType = "noaa-apt"
	SatMeteorLRPT SatelliteType = "meteor-lrpt"
)

// ReceiverTypes maps rnv3 receiver names to their SatDump source, default
// sample rate, and the source parameter carrying the satellite's scalar gain.
// Gain parameter names follow SatDump's per-source options
// (https://docs.satdump.org/md_docs_2pages_2SDR__Options.html) — RN2 passed
// Airspy's --general_gain to every non-RTL device, where it was silently
// ignored. Devices with multi-knob gain (HackRF vga/amp, SDRplay if_gain,
// Airspy HF+ agc_mode/hf_lna) take the rest via the satellite's
// extra_satdump_args.
var ReceiverTypes = map[string]Receiver{
	"rtlsdr":      {Source: "rtlsdr", SampleRate: "1.024e6", GainFlag: "--gain", SupportsPPM: true},
	"airspy_mini": {Source: "airspy", SampleRate: "3e6", GainFlag: "--general_gain"},
	"airspy_r2":   {Source: "airspy", SampleRate: "2.5e6", GainFlag: "--general_gain"},
	// HF+ has no gain, only attenuation (inverted semantics) — mapping the
	// scalar onto it would mislead, so gain is AGC-driven by default and
	// attenuation/agc_mode/hf_lna belong in extra_satdump_args.
	"airspy_hf_plus_discovery": {Source: "airspy", SampleRate: "192e3", GainFlag: ""},
	"hackrf":                   {Source: "hackrf", SampleRate: "4e6", GainFlag: "--lna_gain"},
	"sdrplay":                  {Source: "sdrplay", SampleRate: "2e6", GainFlag: "--lna_gain"},
	"mirisdr":                  {Source: "mirisdr", SampleRate: "2e6", GainFlag: "--gain"},
}

// Receiver describes how a receiver type translates into SatDump flags.
type Receiver struct {
	Source      string
	SampleRate  string
	GainFlag    string // "" = no scalar gain parameter for this device
	SupportsPPM bool
}

type Config struct {
	Station       Station       `yaml:"station"`
	Paths         Paths         `yaml:"paths"`
	SDR           SDR           `yaml:"sdr"`
	Satellites    []Satellite   `yaml:"satellites"`
	Scheduling    Scheduling    `yaml:"scheduling"`
	Capture       Capture       `yaml:"capture"`
	Processing    Processing    `yaml:"processing"`
	Retention     Retention     `yaml:"retention"`
	Daily         Daily         `yaml:"daily"`
	Notifications Notifications `yaml:"notifications"`
	Watchdog      Watchdog      `yaml:"watchdog"`
	Community     Community     `yaml:"community"`
	Web           Web           `yaml:"web"`
	LogLevel      string        `yaml:"log_level"`
}

type Station struct {
	Name      string  `yaml:"name"`     // hostname the panel is served under
	Location  string  `yaml:"location"` // free-text shown in notifications; blank = omitted
	Latitude  float64 `yaml:"latitude"`
	Longitude float64 `yaml:"longitude"`
	Altitude  float64 `yaml:"altitude"` // metres ASL
}

type Paths struct {
	DataDir       string `yaml:"data_dir"`       // db, tle cache, state
	Images        string `yaml:"images"`         // final imagery
	Thumbs        string `yaml:"thumbs"`         // 300px thumbnails
	AudioNOAA     string `yaml:"audio_noaa"`     // retained wav files
	AudioMeteor   string `yaml:"audio_meteor"`   // retained cadu files
	Work          string `yaml:"work"`           // per-pass satdump work dirs
	Ramfs         string `yaml:"ramfs"`          // tmpfs for in-memory capture buffering
	SatdumpBinary string `yaml:"satdump_binary"` //
	SatdumpConfig string `yaml:"satdump_config"` // generated satdump_cfg.json target
}

type SDR struct {
	Type            string `yaml:"type"`              // key into ReceiverTypes
	UseDeviceString bool   `yaml:"use_device_string"` // emit --source_id for multi-dongle setups
}

type Satellite struct {
	Name         string        `yaml:"name"`
	Type         SatelliteType `yaml:"type"`
	NoradID      int           `yaml:"norad_id"`
	FrequencyMHz float64       `yaml:"frequency_mhz"`
	Enabled      bool          `yaml:"enabled"`
	DeviceID     string        `yaml:"sdr_device_id"`
	FreqOffset   float64       `yaml:"freq_offset_ppm"` // RTL-SDR only
	BiasTee      bool          `yaml:"bias_tee"`
	Gain         float64       `yaml:"gain"` // 0 = auto
	// SunMinElevation splits day/night classification for enhancement choice.
	SunMinElevation float64 `yaml:"sun_min_elevation"`
	// MinElevation is the minimum max-elevation for a pass to be scheduled.
	MinElevation float64 `yaml:"min_elevation"`
	// ScheduleSunMinElevation, when set, skips scheduling passes where the sun
	// is below this elevation at pass start (Meteor visible-light gate).
	ScheduleSunMinElevation *float64 `yaml:"schedule_sun_min_elevation,omitempty"`
	// Interleaving80k selects the meteor_m2-x_lrpt_80k pipeline.
	Interleaving80k bool `yaml:"interleaving_80k,omitempty"`
	// SatelliteNumber is SatDump's --satellite_number (NOAA APT only: 15/18/19).
	SatelliteNumber int `yaml:"satellite_number,omitempty"`
	// ExtraSatdumpArgs are appended verbatim to the satdump invocation —
	// the escape hatch for device-specific tuning (e.g. HackRF --vga_gain/
	// --amp, SDRplay --if_gain/--agc_mode, Airspy HF+ --attenuation).
	ExtraSatdumpArgs []string `yaml:"extra_satdump_args,omitempty"`
}

type Scheduling struct {
	DaysAhead            int  `yaml:"days_ahead"`
	ResolveOverlaps      bool `yaml:"resolve_overlaps"`
	PreferMeteorOverNOAA bool `yaml:"prefer_meteor_over_noaa"`
	TLERefreshHourUTC    int  `yaml:"tle_refresh_hour_utc"`
	DryRun               bool `yaml:"dry_run"` // plan passes but never start captures (M1 side-by-side mode)
}

type Capture struct {
	NOAAMemoryThresholdMB   int `yaml:"noaa_memory_threshold_mb"`
	MeteorMemoryThresholdMB int `yaml:"meteor_memory_threshold_mb"`
}

type Processing struct {
	NOAA        NOAAProcessing   `yaml:"noaa"`
	Meteor      MeteorProcessing `yaml:"meteor"`
	PolarAzEl   bool             `yaml:"polar_az_el"`
	PolarDirect bool             `yaml:"polar_direction"`
}

type NOAAProcessing struct {
	DayEnhancements   []string `yaml:"day_enhancements"`
	NightEnhancements []string `yaml:"night_enhancements"`
	JPGQuality        int      `yaml:"jpg_quality"`
	CropWedges        bool     `yaml:"crop_telemetry_wedges"`
	MapCountryBorders bool     `yaml:"map_country_borders"`
}

type MeteorProcessing struct {
	DayEnhancements       []string `yaml:"day_enhancements"`
	NightEnhancements     []string `yaml:"night_enhancements"`
	JPGQuality            int      `yaml:"jpg_quality"`
	FlipNorthbound        bool     `yaml:"flip_northbound"`
	DrawMapOverlay        bool     `yaml:"draw_map_overlay"`
	EquidistantProjection bool     `yaml:"equidistant_projection"`
}

type Retention struct {
	DeleteNOAAAudio          bool `yaml:"delete_noaa_audio"`
	DeleteMeteorAudio        bool `yaml:"delete_meteor_audio"`
	AudioOlderThanDays       int  `yaml:"delete_audio_older_than_days"`
	PruneImagesOlderThanDays int  `yaml:"prune_images_older_than_days"` // 0 = keep forever
}

type Daily struct {
	BestOfDayPush bool `yaml:"best_of_day_push"`
	// PushTime is the local "HH:MM" at which the best-of-day summary goes
	// out (RN2: 22:30 cron).
	PushTime  string    `yaml:"push_time"`
	Timelapse Timelapse `yaml:"timelapse"`
	Mosaic    Mosaic    `yaml:"mosaic"`
}

type Timelapse struct {
	Enabled  bool     `yaml:"enabled"`
	Suffixes []string `yaml:"suffixes"`
}

type Mosaic struct {
	Enabled      bool     `yaml:"enabled"`
	Suffixes     []string `yaml:"suffixes"`
	MinSNR       float64  `yaml:"min_snr"`
	DaylightOnly bool     `yaml:"daylight_only"`
}

type Notifications struct {
	QualityGate QualityGate `yaml:"quality_gate"`
	Webhook     Webhook     `yaml:"webhook"`
	Discord     Discord     `yaml:"discord"`
	Telegram    Telegram    `yaml:"telegram"`
	Pushover    Pushover    `yaml:"pushover"`
	Email       Email       `yaml:"email"`
}

// QualityGate mutes social pushes (not the webhook) for weak passes.
type QualityGate struct {
	Enabled      bool    `yaml:"enabled"`
	MinElevation float64 `yaml:"min_elevation"`
	MinSNR       float64 `yaml:"min_snr"`
}

type Webhook struct {
	Enabled   bool   `yaml:"enabled"`
	URL       string `yaml:"url"`
	AuthToken string `yaml:"auth_token"`
}

type Discord struct {
	Enabled          bool   `yaml:"enabled"`
	NOAAWebhookURL   string `yaml:"noaa_webhook_url"`
	MeteorWebhookURL string `yaml:"meteor_webhook_url"`
}

type Telegram struct {
	Enabled  bool   `yaml:"enabled"`
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

type Pushover struct {
	Enabled  bool   `yaml:"enabled"`
	APIToken string `yaml:"api_token"`
	User     string `yaml:"user"`
	Priority int    `yaml:"priority"`
	LinkURL  string `yaml:"link_url"`
}

type Email struct {
	Enabled      bool   `yaml:"enabled"`
	To           string `yaml:"to"`
	From         string `yaml:"from"`
	SMTPHost     string `yaml:"smtp_host"`
	SMTPPort     int    `yaml:"smtp_port"`
	SMTPUser     string `yaml:"smtp_user"`
	SMTPPassword string `yaml:"smtp_password"`
}

type Watchdog struct {
	Enabled                bool `yaml:"enabled"`
	MaxHoursWithoutCapture int  `yaml:"max_hours_without_capture"`
	DiskUsageThresholdPct  int  `yaml:"disk_usage_threshold_pct"`
}

type Community struct {
	ContributeComposites bool   `yaml:"contribute_composites"`
	URL                  string `yaml:"url"`
}

type Web struct {
	Listen      string      `yaml:"listen"` // e.g. ":80"
	TLS         WebTLS      `yaml:"tls"`
	Admin       WebAdmin    `yaml:"admin"`
	Instruments Instruments `yaml:"instruments"`
	// Panel presentation (RN2: CAPTURES_PER_PAGE, ADMIN_CAPTURES_PER_PAGE,
	// DATE_FORMAT, DATETIME_FORMAT). Formats are Go layouts.
	CapturesPerPage      int    `yaml:"captures_per_page"`
	AdminCapturesPerPage int    `yaml:"admin_captures_per_page"`
	DateFormat           string `yaml:"date_format"`
	DateTimeFormat       string `yaml:"datetime_format"`
}

type WebTLS struct {
	Enabled  bool   `yaml:"enabled"`
	Listen   string `yaml:"listen"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type WebAdmin struct {
	// Enabled locks the admin pages behind a login (RN2: lock_admin_page).
	// When false the admin pages are open, as on a trusted LAN.
	Enabled  bool   `yaml:"enabled"`
	Username string `yaml:"username"`
	// PasswordHash is a bcrypt hash; generate with `rnv3 -hash-password`.
	PasswordHash string `yaml:"password_hash"`
}

type Instruments struct {
	Satvis          bool `yaml:"satvis"`
	SolarTerminator bool `yaml:"solar_terminator"`
	Coronal         bool `yaml:"coronal"`
}

// Default returns the configuration with RN2's shipped defaults mapped over,
// including the five satellites whose frequencies/NORAD ids were previously
// hardcoded in scripts/common.sh and schedule.sh.
func Default() *Config {
	meteorScheduleGate := -6.0
	return &Config{
		Station: Station{
			Name:      "raspberry-noaa.localdomain",
			Latitude:  40.712776,
			Longitude: -74.005974,
			Altitude:  0.0,
		},
		Paths: Paths{
			DataDir:       "/var/lib/rnv3",
			Images:        "/srv/images",
			Thumbs:        "/srv/images/thumb",
			AudioNOAA:     "/srv/audio/noaa",
			AudioMeteor:   "/srv/audio/meteor",
			Work:          "/srv/work",
			Ramfs:         "/var/ramfs",
			SatdumpBinary: "/usr/bin/satdump",
			SatdumpConfig: "/usr/share/satdump/satdump_cfg.json",
		},
		SDR: SDR{Type: "rtlsdr"},
		Satellites: []Satellite{
			{Name: "NOAA 15", Type: SatNOAAAPT, NoradID: 25338, FrequencyMHz: 137.6200, Gain: 29.7, SunMinElevation: 6, MinElevation: 30, SatelliteNumber: 15},
			{Name: "NOAA 18", Type: SatNOAAAPT, NoradID: 28654, FrequencyMHz: 137.9125, Gain: 29.7, SunMinElevation: 6, MinElevation: 30, SatelliteNumber: 18},
			{Name: "NOAA 19", Type: SatNOAAAPT, NoradID: 33591, FrequencyMHz: 137.1000, Gain: 29.7, SunMinElevation: 6, MinElevation: 30, SatelliteNumber: 19},
			{Name: "METEOR-M2 3", Type: SatMeteorLRPT, NoradID: 57166, FrequencyMHz: 137.9000, Enabled: true, Gain: 40.2, SunMinElevation: 6, MinElevation: 30, ScheduleSunMinElevation: &meteorScheduleGate},
			{Name: "METEOR-M2 4", Type: SatMeteorLRPT, NoradID: 59051, FrequencyMHz: 137.9000, Enabled: true, Gain: 40.2, SunMinElevation: 6, MinElevation: 30, ScheduleSunMinElevation: &meteorScheduleGate},
		},
		Scheduling: Scheduling{
			DaysAhead:            7,
			ResolveOverlaps:      true,
			PreferMeteorOverNOAA: true,
			TLERefreshHourUTC:    0,
		},
		Capture: Capture{
			NOAAMemoryThresholdMB:   50,
			MeteorMemoryThresholdMB: 50,
		},
		Processing: Processing{
			NOAA: NOAAProcessing{
				DayEnhancements:   strings.Fields("MSA MSA-precip MCIR MCIR-precip HVC-precip HVCT-precip HVC HVCT ZA therm sea CC HE HF MD BD MB JF JJ LC TA WV NO histeq enhanced-IR"),
				NightEnhancements: strings.Fields("MCIR MCIR-precip HVCT ZA therm NO TA sea histeq enhanced-IR"),
				JPGQuality:        90,
				MapCountryBorders: true,
			},
			Meteor: MeteorProcessing{
				DayEnhancements:       strings.Fields("221 321 124 MSA MCIR MCIR-precip HVC HVCT ZA therm sea CC HE HF MD BD MB JF JJ LC TA WV NO enhanced-IR 39um"),
				NightEnhancements:     strings.Fields("654 456 MCIR MCIR-precip HVC HVCT ZA therm sea CC HE HF MD BD MB JF JJ LC TA WV NO enhanced-IR 39um"),
				JPGQuality:            90,
				FlipNorthbound:        true,
				EquidistantProjection: true,
			},
			PolarAzEl:   true,
			PolarDirect: true,
		},
		Retention: Retention{
			AudioOlderThanDays:       3,
			PruneImagesOlderThanDays: 0,
		},
		Daily: Daily{
			PushTime:  "22:30",
			Timelapse: Timelapse{Suffixes: []string{"-321_projected.jpg", "-221_projected.jpg"}},
			Mosaic: Mosaic{Suffixes: []string{
				"-321_projected.jpg", "-221_projected.jpg",
				"-321_equirect_projected.jpg", "-221_equirect_projected.jpg",
			}},
		},
		Notifications: Notifications{
			Pushover: Pushover{Priority: 0},
			Email:    Email{SMTPPort: 587},
		},
		Watchdog: Watchdog{
			Enabled:                true,
			MaxHoursWithoutCapture: 48,
			DiskUsageThresholdPct:  90,
		},
		Community: Community{URL: "https://voxgalactica.com/upload"},
		Web: Web{
			Listen: ":80",
			TLS:    WebTLS{Listen: ":443"},
			Admin:  WebAdmin{Enabled: false, Username: "admin"},
			Instruments: Instruments{
				Satvis:          true,
				SolarTerminator: true,
			},
			CapturesPerPage:      18,
			AdminCapturesPerPage: 100,
			DateFormat:           "01/02/2006",
			DateTimeFormat:       "01/02/2006 15:04:05",
		},
		LogLevel: "info",
	}
}

// Load reads path, layers it over defaults, and validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks value ranges and cross-field consistency.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.Station.Latitude < -90 || c.Station.Latitude > 90 {
		add("station.latitude %v out of range [-90, 90]", c.Station.Latitude)
	}
	if c.Station.Longitude < -180 || c.Station.Longitude > 180 {
		add("station.longitude %v out of range [-180, 180]", c.Station.Longitude)
	}
	if _, ok := ReceiverTypes[c.SDR.Type]; !ok {
		add("sdr.type %q is not one of %s", c.SDR.Type, strings.Join(receiverNames(), ", "))
	}
	if c.Scheduling.DaysAhead < 1 || c.Scheduling.DaysAhead > 16 {
		add("scheduling.days_ahead %d out of range [1, 16]", c.Scheduling.DaysAhead)
	}
	if h := c.Scheduling.TLERefreshHourUTC; h < 0 || h > 23 {
		add("scheduling.tle_refresh_hour_utc %d out of range [0, 23]", h)
	}
	if len(c.Satellites) == 0 {
		add("satellites: at least one satellite must be defined")
	}
	seen := map[string]bool{}
	enabled := 0
	for i, s := range c.Satellites {
		if s.Name == "" {
			add("satellites[%d]: name is required", i)
		}
		if seen[s.Name] {
			add("satellites[%d]: duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		if s.Type != SatNOAAAPT && s.Type != SatMeteorLRPT {
			add("satellites[%d] (%s): type %q must be %q or %q", i, s.Name, s.Type, SatNOAAAPT, SatMeteorLRPT)
		}
		if s.NoradID <= 0 {
			add("satellites[%d] (%s): norad_id is required", i, s.Name)
		}
		if s.FrequencyMHz < 100 || s.FrequencyMHz > 2000 {
			add("satellites[%d] (%s): frequency_mhz %v looks wrong", i, s.Name, s.FrequencyMHz)
		}
		if s.MinElevation < 0 || s.MinElevation > 90 {
			add("satellites[%d] (%s): min_elevation %v out of range [0, 90]", i, s.Name, s.MinElevation)
		}
		if s.Type == SatNOAAAPT && (s.SatelliteNumber < 1 || s.SatelliteNumber > 99) {
			add("satellites[%d] (%s): satellite_number is required for noaa-apt (SatDump --satellite_number)", i, s.Name)
		}
		if s.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		add("satellites: no satellite is enabled — nothing would ever be captured")
	}
	if q := c.Processing.NOAA.JPGQuality; q < 1 || q > 100 {
		add("processing.noaa.jpg_quality %d out of range [1, 100]", q)
	}
	if q := c.Processing.Meteor.JPGQuality; q < 1 || q > 100 {
		add("processing.meteor.jpg_quality %d out of range [1, 100]", q)
	}
	if _, _, err := ParseClock(c.Daily.PushTime); err != nil {
		add("daily.push_time: %v", err)
	}
	n := c.Notifications
	if n.Webhook.Enabled && n.Webhook.URL == "" {
		add("notifications.webhook.url is required when the webhook is enabled")
	}
	if n.Discord.Enabled && n.Discord.NOAAWebhookURL == "" && n.Discord.MeteorWebhookURL == "" {
		add("notifications.discord: at least one of noaa_webhook_url / meteor_webhook_url is required when enabled")
	}
	if n.Telegram.Enabled && (n.Telegram.BotToken == "" || n.Telegram.ChatID == "") {
		add("notifications.telegram: bot_token and chat_id are required when enabled")
	}
	if n.Pushover.Enabled && (n.Pushover.APIToken == "" || n.Pushover.User == "") {
		add("notifications.pushover: api_token and user are required when enabled")
	}
	if n.Email.Enabled && (n.Email.To == "" || n.Email.From == "" || n.Email.SMTPHost == "") {
		add("notifications.email: to, from and smtp_host are required when enabled")
	}
	if c.Community.ContributeComposites && c.Community.URL == "" {
		add("community.url is required when contribute_composites is enabled")
	}
	if c.Web.Listen == "" {
		add("web.listen must not be empty")
	}
	if c.Web.TLS.Enabled && (c.Web.TLS.CertFile == "" || c.Web.TLS.KeyFile == "") {
		add("web.tls: cert_file and key_file are required when TLS is enabled")
	}
	if c.Web.CapturesPerPage < 1 || c.Web.AdminCapturesPerPage < 1 {
		add("web.captures_per_page and web.admin_captures_per_page must be >= 1")
	}
	if c.Web.DateFormat == "" || c.Web.DateTimeFormat == "" {
		add("web.date_format and web.datetime_format must not be empty")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		add("log_level %q must be debug, info, warn or error", c.LogLevel)
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ParseClock parses a local "HH:MM" time of day into hour and minute.
func ParseClock(s string) (hour, minute int, err error) {
	if _, err := fmt.Sscanf(s, "%d:%d", &hour, &minute); err != nil || len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("%q is not a HH:MM time", s)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is out of range", s)
	}
	return hour, minute, nil
}

// SatelliteByName finds a configured satellite; ok is false when the name is
// not configured (imported history of a retired bird).
func (c *Config) SatelliteByName(name string) (Satellite, bool) {
	for _, s := range c.Satellites {
		if s.Name == name {
			return s, true
		}
	}
	return Satellite{}, false
}

// EnabledSatellites returns the satellites with Enabled set.
func (c *Config) EnabledSatellites() []Satellite {
	var out []Satellite
	for _, s := range c.Satellites {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

func receiverNames() []string {
	names := make([]string, 0, len(ReceiverTypes))
	for k := range ReceiverTypes {
		names = append(names, k)
	}
	return names
}

// Warnings reports settings that are legal but almost certainly not what the
// operator wants. They are logged at startup rather than rejected, so an
// upgrade never leaves the station unable to start.
func (c *Config) Warnings() []string {
	var w []string
	if c.Web.Admin.Enabled && (c.Web.Admin.Username == "" || c.Web.Admin.PasswordHash == "") {
		w = append(w, "web.admin.enabled is true but username/password_hash is not set: the admin pages are locked and no login can succeed until you set password_hash (rnv3 -hash-password) or disable the lock")
	}
	return w
}
