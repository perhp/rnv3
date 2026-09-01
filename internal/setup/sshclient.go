// Package setup implements rnv3-setup: the interactive installer that
// drives a Raspberry Pi over SSH from the operator's PC — probe the Pi,
// ask the questions, generate the config, ship the payload, run the
// installer, and (when asked) cut over from raspberry-noaa-v2.
package setup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client is one SSH connection to the Pi.
type Client struct {
	Host string // host or host:port
	User string
	conn *ssh.Client
}

// ErrHostKeyMismatch: the Pi's host key differs from the remembered one.
var ErrHostKeyMismatch = errors.New("host key mismatch")

// Dial connects with password auth. Unknown host keys are shown to
// askTrust (fingerprint) and, if accepted, appended to knownHostsPath;
// a changed key is refused.
func Dial(host, user, password, knownHostsPath string, askTrust func(host, fingerprint string) bool) (*Client, error) {
	addr := host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(host, "22")
	}
	hostKey := hostKeyCallback(knownHostsPath, askTrust)
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password), ssh.KeyboardInteractive(keyboardPassword(password))},
		HostKeyCallback: hostKey,
		Timeout:         15 * time.Second,
	}
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	return &Client{Host: host, User: user, conn: conn}, nil
}

// keyboardPassword answers keyboard-interactive prompts (some sshd
// configurations use it instead of plain password auth).
func keyboardPassword(password string) ssh.KeyboardInteractiveChallenge {
	return func(name, instruction string, questions []string, echos []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range questions {
			answers[i] = password
		}
		return answers, nil
	}
}

func hostKeyCallback(path string, askTrust func(host, fingerprint string) bool) ssh.HostKeyCallback {
	return func(hostport string, remote net.Addr, key ssh.PublicKey) error {
		if path == "" {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
		check, err := knownhosts.New(path)
		if err != nil {
			return err
		}
		err = check(hostport, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) > 0 {
			return fmt.Errorf("%w for %s: the remembered key differs (%s). If the Pi was reinstalled, delete its line from %s",
				ErrHostKeyMismatch, hostport, ssh.FingerprintSHA256(key), path)
		}
		if askTrust == nil || !askTrust(hostport, ssh.FingerprintSHA256(key)) {
			return fmt.Errorf("host key for %s not trusted", hostport)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostport)}, key))
		return err
	}
}

func (c *Client) Close() error { return c.conn.Close() }

// Result of a remote command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes cmd and collects its output; a non-zero exit is not an
// error (the caller decides), a transport failure is.
func (c *Client) Run(cmd string) (Result, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	code, err := exitCode(sess.Run(cmd))
	return Result{Stdout: out.String(), Stderr: errb.String(), ExitCode: code}, err
}

// RunInput is Run with data on the command's stdin (sudo -S, here-docs).
func (c *Client) RunInput(cmd, stdin string) (Result, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	code, err := exitCode(sess.Run(cmd))
	return Result{Stdout: out.String(), Stderr: errb.String(), ExitCode: code}, err
}

// RunStream executes cmd with both output streams written live to w.
// stopWhen (optional) ends the command early when a line matches, e.g. a
// journal tail that has shown what we were waiting for.
func (c *Client) RunStream(cmd string, w io.Writer, stopWhen func(line string) bool) (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	pr, pw := io.Pipe()
	sess.Stdout, sess.Stderr = pw, pw
	var stopped bool
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(w, line)
			if stopWhen != nil && stopWhen(line) {
				mu.Lock()
				stopped = true
				mu.Unlock()
				sess.Signal(ssh.SIGTERM)
				sess.Close()
				break
			}
		}
		io.Copy(io.Discard, pr)
	}()
	runErr := sess.Run(cmd)
	pw.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if stopped {
		return 0, nil
	}
	return exitCode(runErr)
}

// RunPTY executes cmd under a pseudo-terminal, streaming output to w and
// answering sudo's password prompt (marked promptToken) from stdin. Used
// only when the Pi lacks passwordless sudo.
func (c *Client) RunPTY(cmd string, w io.Writer, promptToken, password string) (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 40, 120, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		return 0, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	sess.Stdout, sess.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		var pending []byte
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				w.Write(chunk)
				pending = append(pending, chunk...)
				if bytes.Contains(pending, []byte(promptToken)) {
					io.WriteString(stdin, password+"\n")
					pending = pending[:0]
				} else if len(pending) > 4096 {
					pending = pending[len(pending)-len(promptToken):]
				}
			}
			if err != nil {
				return
			}
		}
	}()
	runErr := sess.Run(cmd)
	pw.Close()
	<-done
	return exitCode(runErr)
}

// Upload writes data to remotePath (creating parent dirs) with the given
// octal mode, through the session's stdin — no sftp subsystem needed.
func (c *Client) Upload(data []byte, remotePath, mode string) error {
	sess, err := c.conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(data)
	var errb bytes.Buffer
	sess.Stderr = &errb
	q := shellQuote(remotePath)
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s", shellQuote(filepath.ToSlash(remoteDir(remotePath))), q, mode, q)
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("upload %s: %v: %s", remotePath, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// ReadFile fetches a remote file's content ("" and ok=false when absent).
func (c *Client) ReadFile(remotePath string) (string, bool, error) {
	res, err := c.Run("cat " + shellQuote(remotePath) + " 2>/dev/null")
	if err != nil {
		return "", false, err
	}
	if res.ExitCode != 0 {
		return "", false, nil
	}
	return res.Stdout, true, nil
}

func remoteDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "."
	}
	return p[:i]
}

// shellQuote single-quotes s for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus(), nil
	}
	var em *ssh.ExitMissingError
	if errors.As(err, &em) {
		return -1, nil
	}
	return -1, err
}
