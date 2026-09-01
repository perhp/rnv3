//go:build !linux

package jobs

// Development hosts: neither check is probed.
func diskUsagePercent(string) (int, bool) { return 0, false }
func rtlsdrPresent() (bool, bool)         { return false, false }
