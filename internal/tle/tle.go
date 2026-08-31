// Package tle parses, validates, fetches, and caches two-line element sets.
//
// Validation here is strict on purpose: the SGP4 library (go-satellite)
// log.Fatals on malformed lines, so nothing may reach it unchecked — the same
// reason RN2's schedule.sh refused to schedule on unvalidated TLE files.
package tle

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// TLE is one satellite's element set.
type TLE struct {
	Name    string
	NoradID int
	Line1   string
	Line2   string
}

// Set maps NORAD catalog id to element set.
type Set map[int]TLE

// ValidateLines checks structural validity of a TLE pair: line length,
// line numbers, matching catalog ids (and wantID if non-zero), checksums,
// and parseable numeric fields at the columns SGP4 will slice.
func ValidateLines(line1, line2 string, wantID int) error {
	for i, l := range []string{line1, line2} {
		if len(l) != 69 {
			return fmt.Errorf("line %d: length %d, want 69", i+1, len(l))
		}
		if l[0] != byte('1'+i) || l[1] != ' ' {
			return fmt.Errorf("line %d: bad line number prefix %q", i+1, l[:2])
		}
		if got, want := int(l[68]-'0'), Checksum(l[:68]); got != want {
			return fmt.Errorf("line %d: checksum %d, want %d", i+1, got, want)
		}
	}
	id1, err := strconv.Atoi(strings.TrimSpace(line1[2:7]))
	if err != nil {
		return fmt.Errorf("line 1: bad catalog id %q", line1[2:7])
	}
	id2, err := strconv.Atoi(strings.TrimSpace(line2[2:7]))
	if err != nil {
		return fmt.Errorf("line 2: bad catalog id %q", line2[2:7])
	}
	if id1 != id2 {
		return fmt.Errorf("catalog id mismatch: line1=%d line2=%d", id1, id2)
	}
	if wantID != 0 && id1 != wantID {
		return fmt.Errorf("catalog id %d, want %d", id1, wantID)
	}
	// Numeric fields at the exact columns go-satellite slices.
	numeric := []struct {
		line  string
		lo, hi int
		name  string
	}{
		{line1, 18, 20, "epoch year"},
		{line1, 20, 32, "epoch days"},
		{line2, 8, 16, "inclination"},
		{line2, 17, 25, "RAAN"},
		{line2, 34, 42, "arg of perigee"},
		{line2, 43, 51, "mean anomaly"},
		{line2, 52, 63, "mean motion"},
	}
	for _, f := range numeric {
		if _, err := strconv.ParseFloat(strings.TrimSpace(f.line[f.lo:f.hi]), 64); err != nil {
			return fmt.Errorf("%s field %q is not numeric", f.name, f.line[f.lo:f.hi])
		}
	}
	if _, err := strconv.ParseUint(strings.TrimSpace(line2[26:33]), 10, 64); err != nil {
		return fmt.Errorf("eccentricity field %q is not numeric", line2[26:33])
	}
	// The drag/derivative fields go through the SGP4 library's own string
	// assembly before parsing; replicate it exactly so a checksum-valid but
	// malformed TLE cannot reach the library's log.Fatal.
	assembled := []struct {
		value string
		name  string
	}{
		{strings.Replace(line1[33:43], " ", "", 2), "first derivative (ndot)"},
		{strings.Replace(line1[44:45]+"."+line1[45:50]+"e"+line1[50:52], " ", "", 2), "second derivative (nddot)"},
		{strings.Replace(line1[53:54]+"."+line1[54:59]+"e"+line1[59:61], " ", "", 2), "drag term (bstar)"},
	}
	for _, f := range assembled {
		if _, err := strconv.ParseFloat(f.value, 64); err != nil {
			return fmt.Errorf("%s field assembles to %q, not numeric", f.name, f.value)
		}
	}
	return nil
}

// Checksum computes the TLE checksum of a line prefix: digits count as their
// value, '-' counts as 1, everything else as 0, modulo 10.
func Checksum(s string) int {
	sum := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			sum += int(c - '0')
		case c == '-':
			sum++
		}
	}
	return sum % 10
}

// NoradID extracts the catalog id from a validated line.
func NoradID(line string) int {
	id, _ := strconv.Atoi(strings.TrimSpace(line[2:7]))
	return id
}

// ParseSet reads a TLE file: repeated [name line,] line1, line2 groups.
// Invalid groups are returned as an error (a corrupt file must not be
// silently half-used).
func ParseSet(r io.Reader) (Set, error) {
	set := Set{}
	sc := bufio.NewScanner(r)
	var name string
	var line1 string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n ")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "1 "):
			line1 = line
		case strings.HasPrefix(line, "2 "):
			if line1 == "" {
				return nil, fmt.Errorf("line 2 without preceding line 1: %q", line)
			}
			if err := ValidateLines(line1, line, 0); err != nil {
				return nil, err
			}
			id := NoradID(line1)
			set[id] = TLE{Name: strings.TrimSpace(name), NoradID: id, Line1: line1, Line2: line}
			name, line1 = "", ""
		default:
			name = line
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if line1 != "" {
		return nil, fmt.Errorf("dangling line 1 at end of file")
	}
	return set, nil
}

// Format renders the set back to canonical 3-line groups.
func (s Set) Format() string {
	var b strings.Builder
	for _, t := range s.Sorted() {
		name := t.Name
		if name == "" {
			name = fmt.Sprintf("NORAD %d", t.NoradID)
		}
		fmt.Fprintf(&b, "%s\n%s\n%s\n", name, t.Line1, t.Line2)
	}
	return b.String()
}

// Sorted returns the TLEs ordered by NORAD id for deterministic output.
func (s Set) Sorted() []TLE {
	out := make([]TLE, 0, len(s))
	for _, t := range s {
		out = append(out, t)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].NoradID > out[j].NoradID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
