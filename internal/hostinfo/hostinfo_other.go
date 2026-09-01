//go:build !linux

package hostinfo

import "time"

// Development hosts report nothing.
func read(string, time.Duration) Stats { return Stats{} }
