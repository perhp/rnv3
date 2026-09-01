package setup

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

const (
	kUp    = "\x1b[A"
	kDown  = "\x1b[B"
	kLeft  = "\x1b[D"
	kRight = "\x1b[C"
	kEnter = "\r"
)

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// plain strips ANSI escapes and carriage returns for assertions.
func plain(s string) string {
	return strings.ReplaceAll(ansiSeq.ReplaceAllString(s, ""), "\r", "")
}

func TestSelectOneKeys(t *testing.T) {
	opts := []string{"Side by side", "Fresh", "Cut over"}
	cases := []struct {
		keys string
		want int
	}{
		{kEnter, 1},                 // default
		{kDown + kEnter, 2},         // down
		{kDown + kDown + kEnter, 0}, // wraps
		{kUp + kUp + kEnter, 2},     // up wraps from 1 → 0 → 2
		{"3" + kEnter, 2},           // digit jumps
		{"\x1bOA" + kEnter, 0},      // SS3-style arrow
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := selectOne(strings.NewReader(c.keys), &out, "Mode", opts, 1)
		if err != nil || got != c.want {
			t.Errorf("keys %q: got %d (%v), want %d", c.keys, got, err, c.want)
		}
		if !strings.HasSuffix(strings.TrimSpace(plain(out.String())), "Mode: "+opts[c.want]) {
			t.Errorf("keys %q: transcript does not end with the choice:\n%s", c.keys, plain(out.String()))
		}
	}
	if _, err := selectOne(strings.NewReader("\x03"), &bytes.Buffer{}, "M", opts, 0); err != errInterrupted {
		t.Errorf("ctrl-c: %v", err)
	}
	if _, err := selectOne(strings.NewReader("\x1b"), &bytes.Buffer{}, "M", opts, 0); err != errInterrupted {
		t.Errorf("esc: %v", err)
	}
	if _, err := selectOne(strings.NewReader(""), &bytes.Buffer{}, "M", opts, 0); err == nil {
		t.Error("EOF must be an error")
	}
}

func TestSelectManyKeys(t *testing.T) {
	opts := []string{"NOAA 15", "NOAA 18", "NOAA 19", "METEOR-M2 3"}
	var out bytes.Buffer
	// toggle first off, move down twice, toggle NOAA 19 on, accept
	state, err := selectMany(strings.NewReader(" "+kDown+kDown+" "+kEnter), &out, "Satellites", opts, map[string]bool{"NOAA 15": true, "METEOR-M2 3": true})
	if err != nil {
		t.Fatal(err)
	}
	if state["NOAA 15"] || state["NOAA 18"] || !state["NOAA 19"] || !state["METEOR-M2 3"] {
		t.Errorf("state = %v", state)
	}
	if !strings.HasSuffix(strings.TrimSpace(plain(out.String())), "Satellites: NOAA 19, METEOR-M2 3") {
		t.Errorf("transcript:\n%s", plain(out.String()))
	}
	// 'a' toggles all on, then all off; digit toggles one; empty selection reads "none".
	state, _ = selectMany(strings.NewReader("aa2"+kEnter), &out, "S", opts, nil)
	if state["NOAA 15"] || !state["NOAA 18"] {
		t.Errorf("toggle-all/digit state = %v", state)
	}
	out.Reset()
	selectMany(strings.NewReader(kEnter), &out, "S", opts, nil)
	if !strings.Contains(plain(out.String()), "S: none") {
		t.Errorf("empty selection summary:\n%s", plain(out.String()))
	}
	if _, err := selectMany(strings.NewReader("\x03"), &bytes.Buffer{}, "S", opts, nil); err != errInterrupted {
		t.Errorf("ctrl-c: %v", err)
	}
}

func TestConfirmKeys(t *testing.T) {
	cases := []struct {
		keys string
		def  bool
		want bool
	}{
		{kEnter, true, true},
		{kEnter, false, false},
		{kLeft + kEnter, true, false},
		{kRight + kEnter, false, true},
		{"n" + kEnter, true, false},
		{"Y" + kEnter, false, true},
		{" " + kEnter, true, false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := confirm(strings.NewReader(c.keys), &out, "Apply", c.def)
		if err != nil || got != c.want {
			t.Errorf("keys %q def %v: got %v (%v), want %v", c.keys, c.def, got, err, c.want)
		}
		want := "Apply: no"
		if c.want {
			want = "Apply: yes"
		}
		if !strings.HasSuffix(strings.TrimSpace(plain(out.String())), want) {
			t.Errorf("keys %q: transcript:\n%s", c.keys, plain(out.String()))
		}
	}
}

func TestPrompterUsesWidgetsOnlyWithoutCannedAnswer(t *testing.T) {
	// A prompter with widgets wired to a scripted key stream.
	keys := strings.NewReader(kDown + kEnter + "n" + kEnter + " " + kEnter)
	var out bytes.Buffer
	p := &Prompter{Out: &out, Answers: map[string]string{"canned": "two"}, Used: map[string]string{},
		Widgets: &terminalWidgets{in: nil, out: &out}, Fatal: func(err error) { panic("prompter fatal: " + err.Error()) }}
	p.Widgets.rawOverride = func(fn func() error) error { return fn() }
	p.Widgets.reader = keys
	if v := p.AskChoice("canned", "C", []string{"one", "two"}, "one"); v != "two" {
		t.Errorf("canned answer bypassed widgets: %q", v)
	}
	if v := p.AskChoice("pick", "P", []string{"one", "two"}, "one"); v != "two" {
		t.Errorf("widget choice = %q", v)
	}
	if p.AskBool("ok", "OK", true) {
		t.Error("widget confirm should have returned no")
	}
	sel := p.AskMulti("m", "M", []string{"a", "b"}, nil)
	if !sel["a"] || sel["b"] || p.Used["m"] != "a" {
		t.Errorf("widget multi = %v used=%v", sel, p.Used)
	}
}
