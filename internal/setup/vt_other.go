//go:build !windows

package setup

// enableVirtualTerminal is a no-op outside Windows: every other terminal
// speaks ANSI already.
func enableVirtualTerminal() {}
