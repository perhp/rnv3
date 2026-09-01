package setup

import (
	"bufio"
	"io"
	"os"

	"golang.org/x/term"
)

// terminalWidgets is the Prompter's hook into the interactive widgets:
// it puts the terminal in raw mode for the duration of one widget and
// restores it afterwards. Nil when stdin/stdout are not terminals.
type terminalWidgets struct {
	in  *os.File
	out io.Writer
	// test seams: a scripted key stream and a raw-mode substitute
	reader      io.Reader
	rawOverride func(fn func() error) error
	buffered    *bufio.Reader
}

func detectTerminal() *terminalWidgets {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil
	}
	enableVirtualTerminal() // Windows conhost needs VT output switched on
	return &terminalWidgets{in: os.Stdin, out: os.Stdout}
}

// keys returns the one buffered key stream shared by every widget.
func (t *terminalWidgets) keys() io.Reader {
	if t.buffered == nil {
		src := io.Reader(t.in)
		if t.reader != nil {
			src = t.reader
		}
		t.buffered = bufio.NewReader(src)
	}
	return t.buffered
}

// raw runs fn with the terminal in raw mode.
func (t *terminalWidgets) raw(fn func() error) error {
	if t.rawOverride != nil {
		return t.rawOverride(fn)
	}
	state, err := term.MakeRaw(int(t.in.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(t.in.Fd()), state)
	return fn()
}

func (t *terminalWidgets) selectOne(label string, options []string, defIdx int) (int, error) {
	var idx int
	err := t.raw(func() error {
		var e error
		idx, e = selectOne(t.keys(), t.out, label, options, defIdx)
		return e
	})
	return idx, err
}

func (t *terminalWidgets) selectMany(label string, options []string, selected map[string]bool) (map[string]bool, error) {
	var out map[string]bool
	err := t.raw(func() error {
		var e error
		out, e = selectMany(t.keys(), t.out, label, options, selected)
		return e
	})
	return out, err
}

func (t *terminalWidgets) confirm(label string, def bool) (bool, error) {
	var v bool
	err := t.raw(func() error {
		var e error
		v, e = confirm(t.keys(), t.out, label, def)
		return e
	})
	return v, err
}
