//go:build linux

package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// diskUsagePercent reports how full the filesystem holding path is.
func diskUsagePercent(path string) (int, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil || st.Blocks == 0 {
		return 0, false
	}
	used := st.Blocks - st.Bfree
	// df's rounding: used / (used + available), rounded up.
	total := used + st.Bavail
	if total == 0 {
		return 0, false
	}
	return int((used*100 + total - 1) / total), true
}

// rtlsdrPresent scans sysfs for a Realtek RTL2832/RTL2838 USB device
// (what RN2 grepped out of lsusb), without needing usbutils installed.
func rtlsdrPresent() (present bool, probed bool) {
	devices, err := filepath.Glob("/sys/bus/usb/devices/*/idVendor")
	if err != nil || len(devices) == 0 {
		return false, false
	}
	for _, vendorFile := range devices {
		vendor, err := os.ReadFile(vendorFile)
		if err != nil || strings.TrimSpace(string(vendor)) != "0bda" {
			continue
		}
		product, err := os.ReadFile(filepath.Join(filepath.Dir(vendorFile), "idProduct"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(product)) {
		case "2838", "2832":
			return true, true
		}
	}
	return false, true
}
