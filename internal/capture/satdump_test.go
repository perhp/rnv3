package capture

import (
	"strings"
	"testing"
	"time"

	"github.com/perhp/rnv3/internal/config"
)

func baseCfg(receiver string) *config.Config {
	cfg := config.Default()
	cfg.SDR.Type = receiver
	return cfg
}

func noaa19() config.Satellite {
	return config.Satellite{
		Name: "NOAA 19", Type: config.SatNOAAAPT, NoradID: 33591,
		FrequencyMHz: 137.1, Gain: 29.7, SatelliteNumber: 19, FreqOffset: 2,
	}
}

func meteorM23() config.Satellite {
	return config.Satellite{
		Name: "METEOR-M2 3", Type: config.SatMeteorLRPT, NoradID: 57166,
		FrequencyMHz: 137.9, Gain: 40.2,
	}
}

func argString(t *testing.T, cfg *config.Config, sat config.Satellite) string {
	t.Helper()
	args, err := BuildArgs(cfg, sat, "/work/pass-1", 900, 1756640000)
	if err != nil {
		t.Fatal(err)
	}
	return " " + strings.Join(args, " ") + " "
}

func TestBuildArgsNOAAOnRTLSDR(t *testing.T) {
	got := argString(t, baseCfg("rtlsdr"), noaa19())
	for _, want := range []string{
		" live noaa_apt /work/pass-1 ",
		" --source rtlsdr ", " --samplerate 1.024e6 ",
		" --frequency 137.1e6 ",
		" --ppm_correction 2 ",
		" --gain 29.7 ",
		" --satellite_number 19 ",
		" --sdrpp_noise_reduction ",
		" --start_timestamp 1756640000 ",
		" --save_wav ", " --finish_processing ", " --timeout 900 ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:%s", strings.TrimSpace(want), got)
		}
	}
	for _, banned := range []string{"--bias", "--source_id", "--fill_missing", "--general_gain", "--autocrop_wedges"} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q in:%s", banned, got)
		}
	}
}

func TestBuildArgsMeteorOnAirspy(t *testing.T) {
	cfg := baseCfg("airspy_mini")
	sat := meteorM23()
	sat.FreqOffset = 5 // must be ignored: PPM is RTL-SDR-only
	got := argString(t, cfg, sat)
	for _, want := range []string{
		" live meteor_m2-x_lrpt /work/pass-1 ",
		" --source airspy ", " --samplerate 3e6 ",
		" --frequency 137.9e6 ",
		" --general_gain 40.2 ",
		" --fill_missing ", " --finish_processing ", " --timeout 900 ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:%s", strings.TrimSpace(want), got)
		}
	}
	for _, banned := range []string{"--ppm_correction", "--save_wav", "--satellite_number", "--sdrpp_noise_reduction", "--start_timestamp"} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q in:%s", banned, got)
		}
	}
}

func TestBuildArgs80kInterleaving(t *testing.T) {
	sat := meteorM23()
	sat.Interleaving80k = true
	got := argString(t, baseCfg("rtlsdr"), sat)
	if !strings.Contains(got, " live meteor_m2-x_lrpt_80k ") {
		t.Errorf("80k pipeline not selected:%s", got)
	}
}

func TestBuildArgsBiasTeeAndDeviceString(t *testing.T) {
	cfg := baseCfg("rtlsdr")
	cfg.SDR.UseDeviceString = true
	sat := noaa19()
	sat.BiasTee = true
	sat.DeviceID = "00000102"
	got := argString(t, cfg, sat)
	if !strings.Contains(got, " --bias ") {
		t.Errorf("bias tee flag missing:%s", got)
	}
	if !strings.Contains(got, " --source_id 00000102 ") {
		t.Errorf("source_id missing:%s", got)
	}
}

func TestBuildArgsDeviceStringDisabledOmitsSourceID(t *testing.T) {
	sat := noaa19()
	sat.DeviceID = "00000102"
	got := argString(t, baseCfg("rtlsdr"), sat) // UseDeviceString false
	if strings.Contains(got, "--source_id") {
		t.Errorf("source_id must be omitted when use_device_string is false:%s", got)
	}
}

func TestBuildArgsCropWedges(t *testing.T) {
	cfg := baseCfg("rtlsdr")
	cfg.Processing.NOAA.CropWedges = true
	got := argString(t, cfg, noaa19())
	if !strings.Contains(got, " --autocrop_wedges ") {
		t.Errorf("autocrop flag missing:%s", got)
	}
}

func TestBuildArgsAutoGainPassedVerbatim(t *testing.T) {
	sat := noaa19()
	sat.Gain = 0
	got := argString(t, baseCfg("rtlsdr"), sat)
	if !strings.Contains(got, " --gain 0 ") {
		t.Errorf("gain 0 must be passed verbatim (RN2 parity):%s", got)
	}
}

// Per-device gain parameters follow SatDump's SDR options docs — RN2 sent
// Airspy's --general_gain to every non-RTL device, where it did nothing.
func TestBuildArgsGainFlagMatrix(t *testing.T) {
	cases := map[string]string{
		"rtlsdr":      "--gain",
		"airspy_mini": "--general_gain",
		"airspy_r2":   "--general_gain",
		"hackrf":      "--lna_gain",
		"sdrplay":     "--lna_gain",
		"mirisdr":     "--gain",
	}
	for recv, flag := range cases {
		got := argString(t, baseCfg(recv), meteorM23())
		if !strings.Contains(got, " "+flag+" 40.2 ") {
			t.Errorf("%s: gain flag %s missing:%s", recv, flag, got)
		}
	}
}

func TestBuildArgsHFPlusHasNoScalarGain(t *testing.T) {
	got := argString(t, baseCfg("airspy_hf_plus_discovery"), meteorM23())
	for _, banned := range []string{"--gain", "--general_gain", "--lna_gain", "--attenuation"} {
		if strings.Contains(got, " "+banned+" ") {
			t.Errorf("HF+ must not receive %s (attenuation semantics are inverted; use extra_satdump_args):%s", banned, got)
		}
	}
}

func TestBuildArgsExtraSatdumpArgs(t *testing.T) {
	sat := meteorM23()
	sat.ExtraSatdumpArgs = []string{"--vga_gain", "20", "--amp"}
	got := argString(t, baseCfg("hackrf"), sat)
	if !strings.Contains(got, " --vga_gain 20 --amp ") {
		t.Errorf("extra args not appended:%s", got)
	}
}

func TestBuildArgsUnknownReceiver(t *testing.T) {
	cfg := config.Default()
	cfg.SDR.Type = "banana"
	if _, err := BuildArgs(cfg, noaa19(), "/w", 900, 0); err == nil {
		t.Fatal("unknown receiver accepted")
	}
}

func TestFileBaseRN2Compatible(t *testing.T) {
	base := FileBase("METEOR-M2 3", time.Date(2026, 8, 31, 11, 53, 20, 0, time.UTC))
	if base != "METEOR-M2-3-20260831-115320" {
		t.Errorf("file base = %q", base)
	}
}
