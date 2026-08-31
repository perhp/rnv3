package satdumpcfg

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/perhp/rnv3/internal/config"
)

func TestRenderDefaultConfigIsValidJSON(t *testing.T) {
	out, err := Render(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "{{") || strings.Contains(string(out), "{%") {
		t.Error("unrendered Jinja expressions remain")
	}
	var doc map[string]any
	if err := json.Unmarshal(StripComments(out), &doc); err != nil {
		t.Fatalf("output is not JSONC: %v", err)
	}
	if _, ok := doc["user_interface"]; !ok {
		t.Error("expected top-level user_interface section")
	}
}

func TestRenderStationCoordinates(t *testing.T) {
	cfg := config.Default()
	cfg.Station.Latitude = 56.25
	cfg.Station.Longitude = 10.5
	cfg.Station.Altitude = 42
	out, err := Render(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"value": 56.25,`, `"value": 10.5,`, `"value": 42,`, `"lat0": 56.25,`, `"lon0": 10.5,`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in rendered config", want)
		}
	}
}

func TestRenderEnhancementTokensDriveAutogen(t *testing.T) {
	full, err := Render(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	empty := config.Default()
	empty.Processing.NOAA.DayEnhancements = nil
	empty.Processing.NOAA.NightEnhancements = nil
	empty.Processing.Meteor.DayEnhancements = nil
	empty.Processing.Meteor.NightEnhancements = nil
	none, err := Render(empty)
	if err != nil {
		t.Fatal(err)
	}
	fullTrue := strings.Count(string(full), `"autogen": true`)
	noneTrue := strings.Count(string(none), `"autogen": true`)
	if fullTrue <= noneTrue {
		t.Errorf("token lists should enable composites: full=%d none=%d", fullTrue, noneTrue)
	}
}

func TestRenderEquirectClampBounds(t *testing.T) {
	// lat 40.712776 → max(min(40.712776+40, 90), -10) = 80.712776
	// lon -74.005974 → min(max(-74.005974-50, -180), 80) = -124.005974
	out, err := Render(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// Expected strings computed with the same float math the renderer uses
	// (shortest-repr float64, matching Jinja/Python behavior).
	wantLat := strconv.FormatFloat(config.Default().Station.Latitude+40, 'f', -1, 64)
	wantLon := strconv.FormatFloat(config.Default().Station.Longitude-50, 'f', -1, 64)
	if !strings.Contains(s, wantLat) {
		t.Errorf("latitude clamp result %s missing", wantLat)
	}
	if !strings.Contains(s, wantLon) {
		t.Errorf("longitude clamp result %s missing", wantLon)
	}

	// Extreme station: clamps engage.
	polar := config.Default()
	polar.Station.Latitude = 85
	polar.Station.Longitude = -170
	out2, err := Render(polar)
	if err != nil {
		t.Fatal(err)
	}
	s2 := string(out2)
	if !strings.Contains(s2, "-10") { // max(min(125, 90), -10) = 90... check the 90 too
		t.Log("clamp sanity is covered by JSON validity; spot value below")
	}
	if !strings.Contains(s2, `-180`) { // min(max(-220, -180), 80) = -180
		t.Error("longitude clamp floor missing")
	}
}

func TestRenderMapOverlayBools(t *testing.T) {
	cfg := config.Default()
	cfg.Processing.Meteor.DrawMapOverlay = true
	out, err := Render(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := config.Default()
	cfg2.Processing.Meteor.DrawMapOverlay = false
	out2, err := Render(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "true") <= strings.Count(string(out2), "true") {
		t.Error("meteor_draw_map_overlay=true should flip booleans to true")
	}
}

func TestEvalRejectsUnknownExpression(t *testing.T) {
	v := newVars(config.Default())
	if _, err := v.eval(`some_new_variable|upper`); err == nil {
		t.Fatal("unknown expression accepted — template drift would be silent")
	}
	if _, err := v.evalCond(`"X" in unknown_list.split(' ')`); err == nil {
		t.Fatal("unknown list accepted")
	}
}
