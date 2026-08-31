package config

import (
	"path/filepath"
	"runtime"
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
