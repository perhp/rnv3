package tle

import (
	"strings"
	"testing"
)

// fixChecksum appends the correct checksum digit to a 68-char TLE line body.
func fixChecksum(t *testing.T, body string) string {
	t.Helper()
	if len(body) != 68 {
		t.Fatalf("TLE line body must be 68 chars, got %d: %q", len(body), body)
	}
	return body + string(rune('0'+Checksum(body)))
}

// TestLine1/2 model NOAA 19's orbit shape with a 2026-08-31 12:00Z epoch.
const (
	testLine1Body = "1 33591U 09005A   26243.50000000  .00000100  00000-0  60000-4 0  999"
	testLine2Body = "2 33591  99.1900 100.0000 0014000 120.0000 240.0000 14.1200000012345"
)

func testTLE(t *testing.T) TLE {
	t.Helper()
	return TLE{
		Name:    "NOAA 19",
		NoradID: 33591,
		Line1:   fixChecksum(t, testLine1Body),
		Line2:   fixChecksum(t, testLine2Body),
	}
}

func TestValidateLinesAcceptsGoodTLE(t *testing.T) {
	tl := testTLE(t)
	if err := ValidateLines(tl.Line1, tl.Line2, 33591); err != nil {
		t.Fatalf("valid TLE rejected: %v", err)
	}
}

func TestValidateLinesRejectsCorruption(t *testing.T) {
	tl := testTLE(t)
	// Corrupt the SGP4-assembled fields at their exact columns (0-based):
	// ndot [33:43], nddot [44:52], bstar [53:61].
	badNdot := fixChecksum(t, testLine1Body[:33]+" .00X00100"+testLine1Body[43:])
	badNddot := fixChecksum(t, testLine1Body[:44]+" 00Q00-0"+testLine1Body[52:])
	badBstar := fixChecksum(t, testLine1Body[:53]+" 6Z000-4"+testLine1Body[61:])
	cases := map[string][2]string{
		"bad checksum":     {tl.Line1[:68] + "9", tl.Line2},
		"short line":       {tl.Line1[:60], tl.Line2},
		"wrong prefix":     {"3" + tl.Line1[1:], tl.Line2},
		"id mismatch":      {strings.Replace(tl.Line1, "33591", "25338", 1), tl.Line2},
		"garbage numerics": {tl.Line1, fixChecksum(t, testLine2Body[:8] + "XX9.1900" + testLine2Body[16:])},
		"garbage ndot":     {badNdot, tl.Line2},
		"garbage nddot":    {badNddot, tl.Line2},
		"garbage bstar":    {badBstar, tl.Line2},
	}
	for name, lines := range cases {
		if err := ValidateLines(lines[0], lines[1], 33591); err == nil {
			t.Errorf("%s: corruption not detected", name)
		}
	}
}

func TestValidateLinesWrongWantID(t *testing.T) {
	tl := testTLE(t)
	if err := ValidateLines(tl.Line1, tl.Line2, 25338); err == nil {
		t.Fatal("expected error for mismatched wanted catalog id")
	}
}

func TestParseSetRoundTrip(t *testing.T) {
	tl := testTLE(t)
	text := "NOAA 19\n" + tl.Line1 + "\n" + tl.Line2 + "\n"
	set, err := ParseSet(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := set[33591]
	if !ok {
		t.Fatal("NORAD 33591 missing from parsed set")
	}
	if got.Name != "NOAA 19" || got.Line1 != tl.Line1 || got.Line2 != tl.Line2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	set2, err := ParseSet(strings.NewReader(set.Format()))
	if err != nil {
		t.Fatalf("re-parse formatted set: %v", err)
	}
	if len(set2) != 1 || set2[33591].Line1 != tl.Line1 {
		t.Error("Format/ParseSet round-trip lost data")
	}
}

func TestParseSetRejectsCorruptFile(t *testing.T) {
	tl := testTLE(t)
	bad := tl.Line1[:68] + "0" // wrong checksum unless it happens to be 0
	if bad == tl.Line1 {
		bad = tl.Line1[:68] + "1"
	}
	if _, err := ParseSet(strings.NewReader(bad + "\n" + tl.Line2 + "\n")); err == nil {
		t.Fatal("corrupt file accepted")
	}
}
