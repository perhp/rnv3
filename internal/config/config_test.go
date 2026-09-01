package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultValidates(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	example := filepath.Join(thisFile, "..", "..", "..", "config.example.yaml")
	cfg, err := Load(example)
	if err != nil {
		t.Fatalf("config.example.yaml must load: %v", err)
	}
	if got := len(cfg.Satellites); got != 5 {
		t.Errorf("expected 5 satellites, got %d", got)
	}
	if len(cfg.EnabledSatellites()) == 0 {
		t.Error("example config should have at least one enabled satellite")
	}
	// YAML lists replace defaults wholesale, so every satellite block in the
	// example must be complete — a missing min_elevation would silently
	// schedule 0° passes.
	for _, s := range cfg.Satellites {
		if s.MinElevation <= 0 {
			t.Errorf("%s: example config must set min_elevation explicitly", s.Name)
		}
		if s.SunMinElevation == 0 {
			t.Errorf("%s: example config must set sun_min_elevation explicitly", s.Name)
		}
	}
}

func TestValidateCatchesBadValues(t *testing.T) {
	cfg := Default()
	cfg.SDR.Type = "nesdr-ultra"
	cfg.Station.Latitude = 123
	cfg.Satellites[0].MinElevation = 200
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
}

func TestValidateRefreshHourRange(t *testing.T) {
	for _, h := range []int{-1, -48, 24, 100} {
		cfg := Default()
		cfg.Scheduling.TLERefreshHourUTC = h
		if err := cfg.Validate(); err == nil {
			t.Errorf("tle_refresh_hour_utc %d accepted", h)
		}
	}
	cfg := Default()
	cfg.Scheduling.TLERefreshHourUTC = 23
	if err := cfg.Validate(); err != nil {
		t.Errorf("hour 23 rejected: %v", err)
	}
}

func TestRestartOnlyFieldsChanged(t *testing.T) {
	old := Default()
	fresh := Default()
	if got := RestartOnlyFieldsChanged(old, fresh); len(got) != 0 {
		t.Errorf("identical configs flagged: %v", got)
	}
	fresh.Web.Listen = ":8080"
	fresh.Paths.DataDir = "/elsewhere"
	fresh.Scheduling.DryRun = !old.Scheduling.DryRun
	got := RestartOnlyFieldsChanged(old, fresh)
	if len(got) != 3 {
		t.Errorf("changed = %v, want [web.listen paths.data_dir scheduling.dry_run]", got)
	}
	// Runtime-reloadable settings must not be flagged.
	fresh2 := Default()
	fresh2.Satellites[0].Enabled = true
	fresh2.Notifications.Telegram.Enabled = true
	if got := RestartOnlyFieldsChanged(old, fresh2); len(got) != 0 {
		t.Errorf("reloadable changes flagged: %v", got)
	}
}

func TestAdminLockWithoutHashWarnsButLoads(t *testing.T) {
	cfg := Default()
	cfg.Web.Admin.Enabled = true
	cfg.Web.Admin.PasswordHash = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an upgraded config with admin lock but no hash must still load: %v", err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "password_hash") {
		t.Errorf("warnings = %v", w)
	}
	cfg.Web.Admin.PasswordHash = "$2a$10$x"
	if w := cfg.Warnings(); len(w) != 0 {
		t.Errorf("unexpected warnings: %v", w)
	}
}

func TestNotificationValidation(t *testing.T) {
	cfg := Default()
	cfg.Notifications.Telegram.Enabled = true // no token/chat
	cfg.Notifications.Email = Email{Enabled: true, To: "a@b"}
	cfg.Daily.PushTime = "25:00"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"telegram", "email", "push_time"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	cfg = Default()
	cfg.Notifications.Telegram = Telegram{Enabled: true, BotToken: "t", ChatID: "c"}
	cfg.Daily.PushTime = "07:05"
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid channels rejected: %v", err)
	}
	if h, m, _ := ParseClock("07:05"); h != 7 || m != 5 {
		t.Errorf("ParseClock = %d:%d", h, m)
	}
}
