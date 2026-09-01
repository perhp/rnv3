package setup

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakePi is an in-process SSH server that answers a fixed set of exec
// commands, stores uploads, and streams a scripted log.
type fakePi struct {
	addr     string
	password string
	mu       sync.Mutex
	files    map[string][]byte
	commands map[string]string // exact command → stdout (exit 0)
	// handlers get a chance at any command before the exact-match table
	// (installer tests emulate nohup/tail/sudo).
	handlers []func(cmd string, ch ssh.Channel) (handled bool, code int)
	log      []string // every exec'd command, in order
}

var uploadCmd = regexp.MustCompile(`cat > '([^']+)'`)

func startFakePi(t *testing.T) *fakePi {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(key)
	pi := &fakePi{password: "raspberry", files: map[string][]byte{}, commands: map[string]string{}}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "pi" && string(pass) == pi.password {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pi.addr = ln.Addr().String()
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go pi.serve(nc, cfg)
		}
	}()
	return pi
}

func (pi *fakePi) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go pi.session(channel, requests)
	}
}

func (pi *fakePi) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	pty := false
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			pty = true
			req.Reply(true, nil)
		case "exec":
			var payload struct{ Cmd string }
			ssh.Unmarshal(req.Payload, &payload)
			req.Reply(true, nil)
			code := pi.exec(payload.Cmd, ch, pty)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func (pi *fakePi) exec(cmd string, ch ssh.Channel, pty bool) int {
	pi.mu.Lock()
	pi.log = append(pi.log, cmd)
	handlers := pi.handlers
	pi.mu.Unlock()
	for _, h := range handlers {
		if ok, code := h(cmd, ch); ok {
			return code
		}
	}
	if m := uploadCmd.FindStringSubmatch(cmd); m != nil {
		data, _ := io.ReadAll(ch)
		pi.mu.Lock()
		pi.files[m[1]] = data
		pi.mu.Unlock()
		return 0
	}
	switch {
	case strings.HasPrefix(cmd, "cat '"):
		path := strings.TrimSuffix(strings.TrimPrefix(cmd, "cat '"), "' 2>/dev/null")
		pi.mu.Lock()
		data, ok := pi.files[path]
		pi.mu.Unlock()
		if !ok {
			return 1
		}
		ch.Write(data)
		return 0
	case cmd == "stream":
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(ch, "line %d\n", i)
			time.Sleep(20 * time.Millisecond)
		}
		return 0
	case cmd == "endless":
		for i := 1; i <= 200; i++ {
			fmt.Fprintf(ch, "tick %d\n", i)
			time.Sleep(10 * time.Millisecond)
		}
		return 0
	case cmd == "sudo-thing":
		if !pty {
			ch.Stderr().Write([]byte("sudo: a terminal is required\n"))
			return 1
		}
		fmt.Fprint(ch, "RNV3_SUDO_PROMPT")
		line := make([]byte, 64)
		n, _ := ch.Read(line)
		if strings.TrimSpace(string(line[:n])) != pi.password {
			fmt.Fprintln(ch, "Sorry, try again.")
			return 1
		}
		fmt.Fprintln(ch, "\nprivileged work done")
		return 0
	case cmd == "fail":
		ch.Stderr().Write([]byte("boom\n"))
		return 3
	}
	pi.mu.Lock()
	out, ok := pi.commands[cmd]
	pi.mu.Unlock()
	if !ok {
		ch.Stderr().Write([]byte("fake pi: unknown command: " + cmd + "\n"))
		return 127
	}
	ch.Write([]byte(out))
	return 0
}

func dialFake(t *testing.T, pi *fakePi) *Client {
	t.Helper()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	c, err := Dial(pi.addr, "pi", pi.password, kh, func(host, fp string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDialTrustsOnFirstUseAndRefusesChangedKey(t *testing.T) {
	pi := startFakePi(t)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	asked := 0
	c, err := Dial(pi.addr, "pi", "raspberry", kh, func(host, fp string) bool { asked++; return true })
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if asked != 1 {
		t.Errorf("trust prompt shown %d times, want 1", asked)
	}
	raw, _ := os.ReadFile(kh)
	if !strings.Contains(string(raw), "ssh-rsa") {
		t.Error("host key not remembered")
	}
	// Second dial: remembered, no prompt.
	c, err = Dial(pi.addr, "pi", "raspberry", kh, func(host, fp string) bool { asked++; return true })
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if asked != 1 {
		t.Error("remembered host prompted again")
	}
	// A different server on the same known_hosts entry is refused.
	pi2 := startFakePi(t)
	line := strings.Replace(string(raw), strings.Split(strings.TrimSpace(string(raw)), " ")[0], normalizeAddr(pi2.addr), 1)
	os.WriteFile(kh, []byte(line), 0o600)
	if _, err := Dial(pi2.addr, "pi", "raspberry", kh, func(host, fp string) bool { return true }); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Errorf("changed key accepted: %v", err)
	}
	if _, err := Dial(pi.addr, "pi", "wrong", filepath.Join(t.TempDir(), "kh"), func(string, string) bool { return true }); err == nil {
		t.Error("wrong password accepted")
	}
	// Declining trust refuses the connection.
	if _, err := Dial(pi.addr, "pi", "raspberry", filepath.Join(t.TempDir(), "kh2"), func(string, string) bool { return false }); err == nil {
		t.Error("declined host key still connected")
	}
}

func normalizeAddr(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	return "[" + host + "]:" + port
}

func TestRunUploadReadFile(t *testing.T) {
	pi := startFakePi(t)
	pi.commands["uname -m"] = "aarch64\n"
	c := dialFake(t, pi)
	res, err := c.Run("uname -m")
	if err != nil || res.ExitCode != 0 || res.Stdout != "aarch64\n" {
		t.Errorf("run = %+v %v", res, err)
	}
	res, err = c.Run("fail")
	if err != nil || res.ExitCode != 3 || res.Stderr != "boom\n" {
		t.Errorf("failing command = %+v %v", res, err)
	}
	if err := c.Upload([]byte("hello"), "/tmp/rnv3-deploy/deploy/install.sh", "0755"); err != nil {
		t.Fatal(err)
	}
	if got := pi.files["/tmp/rnv3-deploy/deploy/install.sh"]; string(got) != "hello" {
		t.Errorf("uploaded = %q", got)
	}
	body, ok, err := c.ReadFile("/tmp/rnv3-deploy/deploy/install.sh")
	if err != nil || !ok || body != "hello" {
		t.Errorf("read = %q %v %v", body, ok, err)
	}
	if _, ok, _ := c.ReadFile("/nope"); ok {
		t.Error("missing file reported present")
	}
}

func TestRunStreamAndEarlyStop(t *testing.T) {
	pi := startFakePi(t)
	c := dialFake(t, pi)
	var out bytes.Buffer
	code, err := c.RunStream("stream", &out, nil)
	if err != nil || code != 0 || strings.Count(out.String(), "\n") != 5 {
		t.Errorf("stream = %d %v\n%s", code, err, out.String())
	}
	out.Reset()
	start := time.Now()
	code, err = c.RunStream("endless", &out, func(line string) bool { return line == "tick 5" })
	if err != nil || code != 0 || !strings.Contains(out.String(), "tick 5") || time.Since(start) > time.Second {
		t.Errorf("early stop = %d %v after %s\n%s", code, err, time.Since(start), out.String())
	}
}

func TestRunPTYAnswersSudoPrompt(t *testing.T) {
	pi := startFakePi(t)
	c := dialFake(t, pi)
	var out bytes.Buffer
	code, err := c.RunPTY("sudo-thing", &out, "RNV3_SUDO_PROMPT", "raspberry")
	if err != nil || code != 0 || !strings.Contains(out.String(), "privileged work done") {
		t.Errorf("pty = %d %v\n%s", code, err, out.String())
	}
	out.Reset()
	if code, _ := c.RunPTY("sudo-thing", &out, "RNV3_SUDO_PROMPT", "wrong"); code == 0 {
		t.Error("wrong sudo password succeeded")
	}
}
