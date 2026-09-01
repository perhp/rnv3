//go:build !linux

package web

import "time"

// hostVitals has nothing to read outside Linux (development hosts).
func hostVitals(string) map[string]any { return map[string]any{} }

func tzName(now time.Time) string {
	name, _ := now.Zone()
	return name
}
