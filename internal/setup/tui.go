package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Minimal interactive widgets for a real terminal: a single-choice menu,
// a multi-select with checkboxes, and a yes/no toggle, driven by the arrow
// keys, Space and Enter. They read raw key bytes and paint with ANSI
// escapes; when the answer is in, the widget collapses to one
// "label: choice" line so the scrollback stays readable. Prompter falls
// back to the typed prompts when stdin/stdout are not terminals.

// key is one decoded keypress.
type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyEnter
	keySpace
	keyEscape
	keyBackspace
	keyRune // printable; the rune is returned alongside
)

// errInterrupted: Ctrl-C / Esc inside a widget.
var errInterrupted = errors.New("interrupted")

// keyReader decodes keys from a raw-mode terminal (or a scripted byte
// stream in tests).
type keyReader struct{ r *bufio.Reader }

// newKeyReader reuses an existing buffered reader so keys typed ahead of
// one widget are not lost to the next.
func newKeyReader(r io.Reader) *keyReader {
	if br, ok := r.(*bufio.Reader); ok {
		return &keyReader{r: br}
	}
	return &keyReader{r: bufio.NewReader(r)}
}

func (k *keyReader) read() (key, rune, error) {
	b, err := k.r.ReadByte()
	if err != nil {
		return keyNone, 0, err
	}
	switch b {
	case 0x03: // Ctrl-C
		return keyNone, 0, errInterrupted
	case '\r', '\n':
		return keyEnter, 0, nil
	case ' ':
		return keySpace, 0, nil
	case 0x7f, 0x08:
		return keyBackspace, 0, nil
	case 0x1b:
		// ESC alone, or CSI / SS3 sequence for the arrows.
		if k.r.Buffered() == 0 {
			return keyEscape, 0, nil
		}
		next, _ := k.r.ReadByte()
		if next != '[' && next != 'O' {
			return keyEscape, 0, nil
		}
		code, _ := k.r.ReadByte()
		switch code {
		case 'A':
			return keyUp, 0, nil
		case 'B':
			return keyDown, 0, nil
		case 'C':
			return keyRight, 0, nil
		case 'D':
			return keyLeft, 0, nil
		}
		return keyNone, 0, nil
	}
	if b < 0x20 {
		return keyNone, 0, nil
	}
	// Reassemble a UTF-8 rune.
	k.r.UnreadByte()
	r, _, err := k.r.ReadRune()
	if err != nil {
		return keyNone, 0, err
	}
	return keyRune, r, nil
}

// ANSI helpers.
const (
	ansiHideCursor = "\x1b[?25l"
	ansiShowCursor = "\x1b[?25h"
	ansiClearLine  = "\x1b[2K"
	ansiBold       = "\x1b[1m"
	ansiDim        = "\x1b[2m"
	ansiCyan       = "\x1b[36m"
	ansiReset      = "\x1b[0m"
)

// widget paints n lines and repaints them in place on every key.
type widget struct {
	out   io.Writer
	lines int
}

func (w *widget) paint(lines []string) {
	if w.lines > 0 {
		fmt.Fprintf(w.out, "\x1b[%dA", w.lines) // back to the first line
	}
	for _, l := range lines {
		fmt.Fprint(w.out, "\r"+ansiClearLine+l+"\n")
	}
	w.lines = len(lines)
}

// collapse replaces the painted block with a single summary line.
func (w *widget) collapse(summary string) {
	if w.lines > 0 {
		fmt.Fprintf(w.out, "\x1b[%dA", w.lines)
		for i := 0; i < w.lines; i++ {
			fmt.Fprint(w.out, "\r"+ansiClearLine+"\n")
		}
		fmt.Fprintf(w.out, "\x1b[%dA", w.lines)
	}
	fmt.Fprint(w.out, "\r"+ansiClearLine+summary+"\n")
	w.lines = 0
}

// selectOne is the arrow-key menu; returns the chosen index.
func selectOne(in io.Reader, out io.Writer, label string, options []string, defIdx int) (int, error) {
	if defIdx < 0 || defIdx >= len(options) {
		defIdx = 0
	}
	cur := defIdx
	keys := newKeyReader(in)
	w := &widget{out: out}
	fmt.Fprint(out, ansiHideCursor)
	defer fmt.Fprint(out, ansiShowCursor)
	render := func() {
		lines := []string{ansiBold + label + ansiReset + ansiDim + "  (↑/↓ move, Enter select)" + ansiReset}
		for i, o := range options {
			if i == cur {
				lines = append(lines, ansiCyan+"  ❯ "+o+ansiReset)
			} else {
				lines = append(lines, "    "+o)
			}
		}
		w.paint(lines)
	}
	render()
	for {
		k, r, err := keys.read()
		if err != nil {
			return defIdx, err
		}
		switch k {
		case keyUp:
			cur = (cur + len(options) - 1) % len(options)
		case keyDown:
			cur = (cur + 1) % len(options)
		case keyEnter:
			w.collapse(label + ": " + ansiCyan + options[cur] + ansiReset)
			return cur, nil
		case keyEscape:
			return defIdx, errInterrupted
		case keyRune:
			if r >= '1' && r <= '9' && int(r-'1') < len(options) {
				cur = int(r - '1')
			}
		}
		render()
	}
}

// selectMany is the checkbox list; Space toggles, Enter confirms, 'a'
// toggles everything.
func selectMany(in io.Reader, out io.Writer, label string, options []string, selected map[string]bool) (map[string]bool, error) {
	state := map[string]bool{}
	for k, v := range selected {
		state[k] = v
	}
	cur := 0
	keys := newKeyReader(in)
	w := &widget{out: out}
	fmt.Fprint(out, ansiHideCursor)
	defer fmt.Fprint(out, ansiShowCursor)
	render := func() {
		lines := []string{ansiBold + label + ansiReset + ansiDim + "  (↑/↓ move, Space toggle, Enter accept)" + ansiReset}
		for i, o := range options {
			mark := "[ ]"
			if state[o] {
				mark = "[x]"
			}
			if i == cur {
				lines = append(lines, ansiCyan+"  ❯ "+mark+" "+o+ansiReset)
			} else {
				lines = append(lines, "    "+mark+" "+o)
			}
		}
		w.paint(lines)
	}
	render()
	for {
		k, r, err := keys.read()
		if err != nil {
			return state, err
		}
		switch k {
		case keyUp:
			cur = (cur + len(options) - 1) % len(options)
		case keyDown:
			cur = (cur + 1) % len(options)
		case keySpace:
			state[options[cur]] = !state[options[cur]]
		case keyEnter:
			var chosen []string
			for _, o := range options {
				if state[o] {
					chosen = append(chosen, o)
				}
			}
			summary := strings.Join(chosen, ", ")
			if summary == "" {
				summary = "none"
			}
			w.collapse(label + ": " + ansiCyan + summary + ansiReset)
			return state, nil
		case keyEscape:
			return state, errInterrupted
		case keyRune:
			switch {
			case r == 'a' || r == 'A':
				all := true
				for _, o := range options {
					all = all && state[o]
				}
				for _, o := range options {
					state[o] = !all
				}
			case r >= '1' && r <= '9' && int(r-'1') < len(options):
				state[options[r-'1']] = !state[options[r-'1']]
			}
		}
		render()
	}
}

// confirm is the yes/no toggle: ←/→ or y/n move, Enter accepts.
func confirm(in io.Reader, out io.Writer, label string, def bool) (bool, error) {
	cur := def
	keys := newKeyReader(in)
	w := &widget{out: out}
	fmt.Fprint(out, ansiHideCursor)
	defer fmt.Fprint(out, ansiShowCursor)
	render := func() {
		yes, no := "  Yes  ", "  No  "
		if cur {
			yes = ansiCyan + "❯ Yes ❮" + ansiReset
		} else {
			no = ansiCyan + "❯ No ❮" + ansiReset
		}
		w.paint([]string{ansiBold + label + ansiReset + "  " + yes + " " + no + ansiDim + "  (←/→, y/n, Enter)" + ansiReset})
	}
	render()
	for {
		k, r, err := keys.read()
		if err != nil {
			return def, err
		}
		switch k {
		case keyLeft, keyRight, keyUp, keyDown, keySpace:
			cur = !cur
		case keyEnter:
			answer := "no"
			if cur {
				answer = "yes"
			}
			w.collapse(label + ": " + ansiCyan + answer + ansiReset)
			return cur, nil
		case keyEscape:
			return def, errInterrupted
		case keyRune:
			switch unicode.ToLower(r) {
			case 'y':
				cur = true
			case 'n':
				cur = false
			}
		}
		render()
	}
}
