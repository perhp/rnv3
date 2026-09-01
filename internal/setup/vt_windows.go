//go:build windows

package setup

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal turns on ANSI escape processing and UTF-8 output
// for the classic Windows console (Windows Terminal has both already), so
// the menus' arrows and check marks render.
func enableVirtualTerminal() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err == nil {
		windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	windows.SetConsoleOutputCP(65001)
}
