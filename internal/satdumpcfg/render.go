// Package satdumpcfg renders SatDump's satdump_cfg.json from the station
// config. The 5,308-line template is RN2's satdump_cfg.json.j2 embedded
// VERBATIM (zero transcription risk, diffable against upstream); this
// mini-renderer implements exactly the closed set of Jinja constructs the
// template uses and fails loudly on anything else, so template drift is
// caught instead of silently mis-rendered.
//
// Supported constructs (the complete vocabulary, catalogued from the file):
//
//	{{ latitude }} {{ longitude }} {{ altitude }}
//	{{ <boolvar>|lower }}
//	{{ "true" if <cond> else "false" }}
//	  cond: "TOK" in <listvar>.split(' ')
//	      | (<cond> and meteor_create_equidistant_projection|string|lower == 'true')
//	      | (<cond> or <cond>)
//	      | meteor_create_equidistant_projection|string|lower == 'true'
//	{{ [([latitude + 40, 90] | min), -10] | max }}
//	{{ [([longitude - 50, -180] | max), 80] | min }}
package satdumpcfg

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/process"
)

//go:embed satdump_cfg.json.j2
var template string

var exprPattern = regexp.MustCompile(`\{\{(.+?)\}\}|\{%(.+?)%\}`)

// Render produces the satdump_cfg.json content for the station config and
// validates that the result is valid JSON.
func Render(cfg *config.Config) ([]byte, error) {
	vars := newVars(cfg)
	var renderErr error
	out := exprPattern.ReplaceAllStringFunc(template, func(m string) string {
		if strings.HasPrefix(m, "{%") {
			renderErr = fmt.Errorf("unsupported Jinja block %q — template drifted beyond the renderer's vocabulary", m)
			return m
		}
		v, err := vars.eval(strings.TrimSpace(m[2 : len(m)-2]))
		if err != nil && renderErr == nil {
			renderErr = err
		}
		return v
	})
	if renderErr != nil {
		return nil, renderErr
	}
	// The file is JSONC — SatDump's parser accepts // comments and RN2
	// shipped it with them — so validate a comment-stripped copy but write
	// the original.
	if !json.Valid(StripComments([]byte(out))) {
		return nil, fmt.Errorf("rendered satdump_cfg.json is not valid JSON (after comment stripping)")
	}
	return []byte(out), nil
}

// StripComments removes // line comments outside of strings, for validating
// SatDump's JSONC config with a strict JSON parser.
func StripComments(in []byte) []byte {
	out := make([]byte, 0, len(in))
	inString := false
	escaped := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(in) && in[i+1] == '/' {
			for i < len(in) && in[i] != '\n' {
				i++
			}
			if i < len(in) {
				out = append(out, '\n')
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

// Sync renders and writes the config file, only touching it when the content
// differs. Returns whether it wrote.
func Sync(cfg *config.Config) (bool, error) {
	rendered, err := Render(cfg)
	if err != nil {
		return false, err
	}
	path := cfg.Paths.SatdumpConfig
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(rendered) {
		return false, nil
	}
	if err := process.WriteFileAtomic(path, rendered); err != nil {
		return false, err
	}
	return true, nil
}

type vars struct {
	lat, lon, alt float64
	bools         map[string]bool
	lists         map[string]map[string]bool
}

func newVars(cfg *config.Config) *vars {
	toSet := func(tokens []string) map[string]bool {
		s := map[string]bool{}
		for _, t := range tokens {
			s[t] = true
		}
		return s
	}
	return &vars{
		lat: cfg.Station.Latitude,
		lon: cfg.Station.Longitude,
		alt: cfg.Station.Altitude,
		bools: map[string]bool{
			"meteor_draw_map_overlay":              cfg.Processing.Meteor.DrawMapOverlay,
			"noaa_map_country_border_enable":       cfg.Processing.NOAA.MapCountryBorders,
			"meteor_create_equidistant_projection": cfg.Processing.Meteor.EquidistantProjection,
		},
		lists: map[string]map[string]bool{
			"noaa_daytime_enhancements":     toSet(cfg.Processing.NOAA.DayEnhancements),
			"noaa_nighttime_enhancements":   toSet(cfg.Processing.NOAA.NightEnhancements),
			"meteor_daytime_enhancements":   toSet(cfg.Processing.Meteor.DayEnhancements),
			"meteor_nighttime_enhancements": toSet(cfg.Processing.Meteor.NightEnhancements),
		},
	}
}

var (
	condExpr  = regexp.MustCompile(`^"true" if (.+) else "false"$`)
	inExpr    = regexp.MustCompile(`^"([^"]+)" in ([a-z_]+)\.split\(' '\)$`)
	lowerExpr = regexp.MustCompile(`^([a-z_]+)\|lower$`)
	equiExpr  = regexp.MustCompile(`^meteor_create_equidistant_projection\|string\|lower == 'true'$`)
	clampExpr = regexp.MustCompile(`^\[\(\[(latitude|longitude) ([+-]) (\d+), (-?\d+)\] \| (min|max)\), (-?\d+)\] \| (min|max)$`)
)

func (v *vars) eval(expr string) (string, error) {
	switch expr {
	case "latitude":
		return formatNum(v.lat), nil
	case "longitude":
		return formatNum(v.lon), nil
	case "altitude":
		return formatNum(v.alt), nil
	}
	if m := lowerExpr.FindStringSubmatch(expr); m != nil {
		b, ok := v.bools[m[1]]
		if !ok {
			return "", fmt.Errorf("unknown bool variable %q", m[1])
		}
		return strconv.FormatBool(b), nil
	}
	if m := condExpr.FindStringSubmatch(expr); m != nil {
		b, err := v.evalCond(strings.TrimSpace(m[1]))
		if err != nil {
			return "", err
		}
		return strconv.FormatBool(b), nil
	}
	if m := clampExpr.FindStringSubmatch(expr); m != nil {
		return v.evalClamp(m)
	}
	return "", fmt.Errorf("unsupported template expression %q", expr)
}

func (v *vars) evalCond(cond string) (bool, error) {
	cond = strings.TrimSpace(cond)
	// Strip one level of wrapping parens if they enclose the whole condition.
	if strings.HasPrefix(cond, "(") && strings.HasSuffix(cond, ")") && balanced(cond[1:len(cond)-1]) {
		cond = strings.TrimSpace(cond[1 : len(cond)-1])
	}
	if i := indexTopLevel(cond, " and "); i >= 0 {
		l, err := v.evalCond(cond[:i])
		if err != nil {
			return false, err
		}
		r, err := v.evalCond(cond[i+5:])
		return l && r, err
	}
	if i := indexTopLevel(cond, " or "); i >= 0 {
		l, err := v.evalCond(cond[:i])
		if err != nil {
			return false, err
		}
		r, err := v.evalCond(cond[i+4:])
		return l || r, err
	}
	if m := inExpr.FindStringSubmatch(cond); m != nil {
		list, ok := v.lists[m[2]]
		if !ok {
			return false, fmt.Errorf("unknown enhancement list %q", m[2])
		}
		return list[m[1]], nil
	}
	if equiExpr.MatchString(cond) {
		return v.bools["meteor_create_equidistant_projection"], nil
	}
	return false, fmt.Errorf("unsupported template condition %q", cond)
}

// evalClamp handles the two equirect-bounds expressions:
//
//	[([latitude + 40, 90] | min), -10] | max   → max(min(lat+40, 90), -10)
//	[([longitude - 50, -180] | max), 80] | min → min(max(lon-50, -180), 80)
func (v *vars) evalClamp(m []string) (string, error) {
	base := v.lat
	if m[1] == "longitude" {
		base = v.lon
	}
	delta, _ := strconv.ParseFloat(m[3], 64)
	if m[2] == "-" {
		delta = -delta
	}
	inner, _ := strconv.ParseFloat(m[4], 64)
	outer, _ := strconv.ParseFloat(m[6], 64)

	val := base + delta
	switch m[5] {
	case "min":
		val = math.Min(val, inner)
	case "max":
		val = math.Max(val, inner)
	}
	switch m[7] {
	case "min":
		val = math.Min(val, outer)
	case "max":
		val = math.Max(val, outer)
	}
	return formatNum(val), nil
}

// indexTopLevel finds sep outside any parentheses.
func indexTopLevel(s, sep string) int {
	depth := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}

// balanced reports whether parens in s are balanced and never negative.
func balanced(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func formatNum(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
