package setup

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Prompter asks ssh-keygen-style questions: label, default in brackets,
// Enter accepts. Every question has a key; a value under that key in
// Answers (from --answers) is used without asking, so a whole run can be
// scripted. An invalid canned value is reported and dropped, after which
// the question is asked interactively; when the input is exhausted (EOF)
// the run fails instead of looping.
type Prompter struct {
	In      *bufio.Reader
	Out     io.Writer
	Answers map[string]string
	// ReadSecret reads without echo; nil falls back to plain input (tests).
	ReadSecret func() (string, error)
	// Used records every answer given, for --save-answers.
	Used map[string]string
	// Fatal ends the run on unrecoverable input; nil prints and exits 2.
	Fatal func(err error)

	eof bool
}

// NewPrompter wires stdin/stdout with hidden secret input on a terminal.
func NewPrompter(answers map[string]string) *Prompter {
	p := &Prompter{In: bufio.NewReader(os.Stdin), Out: os.Stdout, Answers: answers, Used: map[string]string{}}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		p.ReadSecret = func() (string, error) {
			raw, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stdout)
			return string(raw), err
		}
	}
	return p
}

func (p *Prompter) readLine() string {
	line, err := p.In.ReadString('\n')
	if err != nil {
		p.eof = true
		if line == "" {
			return ""
		}
	}
	return strings.TrimRight(line, "\r\n")
}

func (p *Prompter) fail(err error) {
	if p.Fatal != nil {
		p.Fatal(err)
		return
	}
	fmt.Fprintln(os.Stderr, "\nerror:", err)
	os.Exit(2)
}

func (p *Prompter) record(key, value string) {
	if p.Used != nil {
		p.Used[key] = value
	}
}

// canned returns a scripted answer and whether one existed.
func (p *Prompter) canned(key string) (string, bool) {
	v, ok := p.Answers[key]
	return v, ok
}

// rejectCanned drops an invalid scripted answer so the question falls
// through to interactive input, or fails when there is none.
func (p *Prompter) rejectCanned(key, value, why string) {
	fmt.Fprintf(p.Out, "  answers file: %s = %q is invalid (%s)\n", key, value, why)
	delete(p.Answers, key)
	if p.Exhausted() {
		p.fail(fmt.Errorf("answers file: %s = %q is invalid (%s) and there is no interactive input", key, value, why))
	}
}

// Exhausted reports that interactive input has ended (peeks, so it is
// accurate before the first read too).
func (p *Prompter) Exhausted() bool {
	if p.eof {
		return true
	}
	if _, err := p.In.Peek(1); err != nil {
		p.eof = true
	}
	return p.eof
}

// Ask returns a string answer; empty input keeps the default.
func (p *Prompter) Ask(key, label, def string) string {
	if v, ok := p.canned(key); ok {
		fmt.Fprintf(p.Out, "%s: %s  (from answers)\n", label, v)
		p.record(key, v)
		return v
	}
	if p.eof { // no more input: Enter
		p.record(key, def)
		return def
	}
	if def != "" {
		fmt.Fprintf(p.Out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.Out, "%s: ", label)
	}
	line := strings.TrimSpace(p.readLine())
	if line == "" {
		line = def
	}
	p.record(key, line)
	return line
}

// AskRequired repeats until a non-empty answer is given.
func (p *Prompter) AskRequired(key, label, def string) string {
	for {
		if v, ok := p.canned(key); ok && strings.TrimSpace(v) == "" {
			p.rejectCanned(key, v, "a value is required")
			continue
		}
		if v := p.Ask(key, label, def); v != "" {
			return v
		}
		fmt.Fprintln(p.Out, "  a value is required")
		if p.eof {
			p.fail(fmt.Errorf("no value for %s", key))
			return ""
		}
	}
}

// AskBool: y/n with a default.
func (p *Prompter) AskBool(key, label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		raw, isCanned := p.canned(key)
		v := strings.ToLower(strings.TrimSpace(p.Ask(key, label+" ("+hint+")", "")))
		switch v {
		case "":
			p.record(key, strconv.FormatBool(def))
			return def
		case "y", "yes", "true":
			p.record(key, "true")
			return true
		case "n", "no", "false":
			p.record(key, "false")
			return false
		}
		if isCanned {
			p.rejectCanned(key, raw, "expected y or n")
			continue
		}
		fmt.Fprintln(p.Out, "  please answer y or n")
		if p.eof {
			p.fail(fmt.Errorf("no valid answer for %s", key))
			return def
		}
	}
}

func (p *Prompter) AskFloat(key, label string, def float64) float64 {
	for {
		raw, isCanned := p.canned(key)
		v := p.Ask(key, label, strconv.FormatFloat(def, 'f', -1, 64))
		if f, err := strconv.ParseFloat(strings.Replace(v, ",", ".", 1), 64); err == nil {
			return f
		}
		if isCanned {
			p.rejectCanned(key, raw, "expected a number")
			continue
		}
		fmt.Fprintln(p.Out, "  please enter a number")
		if p.eof {
			p.fail(fmt.Errorf("no valid answer for %s", key))
			return def
		}
	}
}

func (p *Prompter) AskInt(key, label string, def int) int {
	for {
		raw, isCanned := p.canned(key)
		v := p.Ask(key, label, strconv.Itoa(def))
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		if isCanned {
			p.rejectCanned(key, raw, "expected a whole number")
			continue
		}
		fmt.Fprintln(p.Out, "  please enter a whole number")
		if p.eof {
			p.fail(fmt.Errorf("no valid answer for %s", key))
			return def
		}
	}
}

// AskSecret reads a hidden value; empty keeps the existing one when
// hasExisting (shown as "unchanged"). Canned values are never echoed.
func (p *Prompter) AskSecret(key, label string, hasExisting bool) string {
	if v, ok := p.canned(key); ok {
		fmt.Fprintf(p.Out, "%s: ********  (from answers)\n", label)
		return v
	}
	if p.eof {
		return "" // the caller treats it as "keep / none"
	}
	if hasExisting {
		fmt.Fprintf(p.Out, "%s [unchanged]: ", label)
	} else {
		fmt.Fprintf(p.Out, "%s: ", label)
	}
	if p.ReadSecret != nil {
		v, _ := p.ReadSecret()
		return v
	}
	return strings.TrimSpace(p.readLine())
}

// AskChoice picks one option by number or exact name.
func (p *Prompter) AskChoice(key, label string, options []string, def string) string {
	if v, ok := p.canned(key); ok {
		for _, o := range options {
			if o == v {
				fmt.Fprintf(p.Out, "%s: %s  (from answers)\n", label, v)
				p.record(key, v)
				return v
			}
		}
		p.rejectCanned(key, v, "not one of "+strings.Join(options, ", "))
	}
	fmt.Fprintf(p.Out, "%s:\n", label)
	defIdx := 0
	for i, o := range options {
		marker := " "
		if o == def {
			marker = "*"
			defIdx = i + 1
		}
		fmt.Fprintf(p.Out, "  %s%d) %s\n", marker, i+1, o)
	}
	for {
		hint := ""
		if defIdx > 0 {
			hint = fmt.Sprintf(" [%d]", defIdx)
		}
		fmt.Fprintf(p.Out, "choice%s: ", hint)
		line := strings.TrimSpace(p.readLine())
		if line == "" && defIdx > 0 {
			p.record(key, def)
			return def
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			p.record(key, options[n-1])
			return options[n-1]
		}
		for _, o := range options {
			if strings.EqualFold(o, line) {
				p.record(key, o)
				return o
			}
		}
		fmt.Fprintln(p.Out, "  pick a number from the list")
		if p.eof {
			p.fail(fmt.Errorf("no valid choice for %s", key))
			return def
		}
	}
}

// AskMulti toggles a set: shows the options with [x] marks, accepts a
// comma/space list of numbers to toggle, Enter to confirm.
func (p *Prompter) AskMulti(key, label string, options []string, selected map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range selected {
		out[k] = v
	}
	if v, ok := p.canned(key); ok {
		valid := map[string]bool{}
		for _, o := range options {
			valid[o] = true
		}
		chosen := map[string]bool{}
		bad := ""
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				if !valid[name] {
					bad = name
					break
				}
				chosen[name] = true
			}
		}
		if bad != "" {
			p.rejectCanned(key, v, "unknown option "+bad)
		} else {
			for k := range out {
				out[k] = false
			}
			for k := range chosen {
				out[k] = true
			}
			fmt.Fprintf(p.Out, "%s: %s  (from answers)\n", label, v)
			p.record(key, v)
			return out
		}
	}
	for {
		fmt.Fprintf(p.Out, "%s (enter numbers to toggle, Enter to accept):\n", label)
		for i, o := range options {
			mark := " "
			if out[o] {
				mark = "x"
			}
			fmt.Fprintf(p.Out, "  [%s] %d) %s\n", mark, i+1, o)
		}
		fmt.Fprint(p.Out, "toggle: ")
		line := strings.TrimSpace(p.readLine())
		if line == "" {
			var chosen []string
			for _, o := range options {
				if out[o] {
					chosen = append(chosen, o)
				}
			}
			p.record(key, strings.Join(chosen, ","))
			return out
		}
		for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
			if n, err := strconv.Atoi(tok); err == nil && n >= 1 && n <= len(options) {
				out[options[n-1]] = !out[options[n-1]]
			}
		}
		if p.eof {
			// No more input: accept the current selection.
			var chosen []string
			for _, o := range options {
				if out[o] {
					chosen = append(chosen, o)
				}
			}
			p.record(key, strings.Join(chosen, ","))
			return out
		}
	}
}

// Say prints a line to the operator.
func (p *Prompter) Say(format string, a ...any) { fmt.Fprintf(p.Out, format+"\n", a...) }
