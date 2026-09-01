package setup

import (
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Payload is what gets shipped to the Pi (embedded in rnv3-setup or read
// from --payload-dir).
type Payload struct {
	RNV3        []byte // linux/arm64 rnv3
	Migrate     []byte // linux/arm64 rnv3-migrate
	InstallSH   []byte
	CutoverSH   []byte
	Service     []byte
	ExampleYAML []byte
}

func (p Payload) Check() error {
	var missing []string
	for name, data := range map[string][]byte{"rnv3": p.RNV3, "rnv3-migrate": p.Migrate, "install.sh": p.InstallSH,
		"cutover.sh": p.CutoverSH, "rnv3.service": p.Service, "config.example.yaml": p.ExampleYAML} {
		if len(data) == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("payload incomplete (%s): build rnv3-setup with `go run ./tools/release`, or pass --payload-dir", strings.Join(missing, ", "))
	}
	return nil
}

const (
	remoteDeployDir = "/tmp/rnv3-deploy"
	sudoPromptToken = "RNV3_SUDO_PROMPT"
	exitMarker      = "RNV3_EXIT="
)

var exitMarkerRe = regexp.MustCompile(`RNV3_EXIT=(-?\d+)`)

// Installer runs the remote steps. All output streams to Out.
type Installer struct {
	C        *Client
	Probe    *Probe
	Payload  Payload
	Password string // the SSH password, reused for sudo when it is not passwordless
	Out      io.Writer
}

// sudo runs one privileged command.
func (in *Installer) sudo(cmd string) (Result, error) {
	if in.Probe.SudoNoPass {
		return in.C.Run("sudo -n " + cmd)
	}
	return in.C.RunInput("sudo -S -p '' "+cmd, in.Password+"\n")
}

func (in *Installer) mustSudo(cmd string) error {
	res, err := in.sudo(cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sudo %s: exit %d: %s", cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ShipPayload uploads binaries and scripts to /tmp/rnv3-deploy.
func (in *Installer) ShipPayload() error {
	if err := in.Payload.Check(); err != nil {
		return err
	}
	fmt.Fprintln(in.Out, "==> Uploading rnv3 payload")
	files := []struct {
		name string
		data []byte
		mode string
	}{
		{"rnv3", in.Payload.RNV3, "0755"},
		{"rnv3-migrate", in.Payload.Migrate, "0755"},
		{"config.example.yaml", in.Payload.ExampleYAML, "0644"},
		{"deploy/install.sh", in.Payload.InstallSH, "0755"},
		{"deploy/cutover.sh", in.Payload.CutoverSH, "0755"},
		{"deploy/rnv3.service", in.Payload.Service, "0644"},
	}
	if _, err := in.C.Run("rm -rf " + remoteDeployDir + " && mkdir -p " + remoteDeployDir + "/deploy"); err != nil {
		return err
	}
	for _, f := range files {
		fmt.Fprintf(in.Out, "    %s (%d bytes)\n", f.name, len(f.data))
		if err := in.C.Upload(f.data, remoteDeployDir+"/"+f.name, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// WriteConfig installs the generated config as /etc/rnv3/config.yaml,
// 0600 and owned by the station user.
func (in *Installer) WriteConfig(yamlText []byte) error {
	fmt.Fprintln(in.Out, "==> Writing /etc/rnv3/config.yaml")
	if err := in.C.Upload(yamlText, remoteDeployDir+"/config.yaml", "0600"); err != nil {
		return err
	}
	user := in.C.User
	return in.mustSudo(fmt.Sprintf("sh -c 'mkdir -p /etc/rnv3 && install -m 0600 -o %s -g %s %s/config.yaml /etc/rnv3/config.yaml'",
		user, user, remoteDeployDir))
}

// runScript executes a deploy script with live output. With passwordless
// sudo it runs detached under nohup (a dropped Wi-Fi link cannot kill an
// hour-long SatDump build) and the log is tailed; otherwise it runs under a
// pty, sudo is pre-authenticated with the password and kept alive.
func (in *Installer) runScript(script string) error {
	if in.Probe.SudoNoPass {
		start := fmt.Sprintf("cd %s && nohup sh -c '%s; echo %s$?' > %s/run.log 2>&1 < /dev/null & echo $!",
			remoteDeployDir, script, exitMarker, remoteDeployDir)
		res, err := in.C.Run(start)
		if err != nil {
			return err
		}
		pid := strings.TrimSpace(res.Stdout)
		if _, err := strconv.Atoi(pid); err != nil {
			return fmt.Errorf("could not start %s: %s %s", script, res.Stdout, res.Stderr)
		}
		exit := -1
		_, err = in.C.RunStream(fmt.Sprintf("tail -n +1 --pid=%s -f %s/run.log", pid, remoteDeployDir), in.Out, func(line string) bool {
			if m := exitMarkerRe.FindStringSubmatch(line); m != nil {
				exit, _ = strconv.Atoi(m[1])
				return true
			}
			return false
		})
		if err != nil {
			return err
		}
		if exit != 0 {
			return fmt.Errorf("%s exited with status %d (log: %s/run.log)", script, exit, remoteDeployDir)
		}
		return nil
	}

	cmd := fmt.Sprintf("sudo -S -p '%s' -v && (while sudo -n -v; do sleep 50; done 2>/dev/null &) ; cd %s && %s; rc=$?; sudo -k; echo %s$rc",
		sudoPromptToken, remoteDeployDir, script, exitMarker)
	var tail lastLines
	code, err := in.C.RunPTY(cmd, io.MultiWriter(in.Out, &tail), sudoPromptToken, in.Password)
	if err != nil {
		return err
	}
	if m := exitMarkerRe.FindStringSubmatch(tail.String()); m != nil {
		if n, _ := strconv.Atoi(m[1]); n != 0 {
			return fmt.Errorf("%s exited with status %d", script, n)
		}
		return nil
	}
	if code != 0 {
		return fmt.Errorf("%s: session exited with status %d", script, code)
	}
	return nil
}

// lastLines keeps the tail of a stream for marker parsing.
type lastLines struct{ buf []byte }

func (l *lastLines) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	if len(l.buf) > 4096 {
		l.buf = l.buf[len(l.buf)-4096:]
	}
	return len(p), nil
}
func (l *lastLines) String() string { return string(l.buf) }

// RunInstall runs deploy/install.sh. noStart leaves the service stopped
// (so a history import can run before the first start).
func (in *Installer) RunInstall(noStart bool) error {
	fmt.Fprintln(in.Out, "==> Running install.sh on the Pi")
	if in.Probe.SatDump == "" || in.Probe.RTLSDRBin == "" {
		fmt.Fprintln(in.Out, "    building missing dependencies from source — this takes a long time on a Pi; the build survives a dropped connection")
	}
	args := "./rnv3"
	if noStart {
		args = "--no-start " + args
	}
	return in.runScript("./deploy/install.sh " + args)
}

// ImportHistory runs rnv3-migrate against RN2's panel.db with rnv3 stopped.
func (in *Installer) ImportHistory() error {
	fmt.Fprintln(in.Out, "==> Importing raspberry-noaa-v2 history")
	if err := in.mustSudo("systemctl stop rnv3"); err != nil {
		return err
	}
	code, err := in.C.RunStream(fmt.Sprintf("/usr/local/bin/rnv3-migrate -old %s/db/panel.db -config /etc/rnv3/config.yaml", shellQuote(in.Probe.RN2Home)), in.Out, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("rnv3-migrate exited with status %d", code)
	}
	return nil
}

// Service control.
func (in *Installer) Start() error   { return in.mustSudo("systemctl start rnv3") }
func (in *Installer) Restart() error { return in.mustSudo("systemctl restart rnv3") }
func (in *Installer) Reload() error  { return in.mustSudo("systemctl reload rnv3") }

// WaitForPlan tails the journal until the scheduler reports its first plan
// (or a planning failure), for up to timeout.
func (in *Installer) WaitForPlan(timeout time.Duration) (ok bool, err error) {
	fmt.Fprintln(in.Out, "==> Waiting for the first pass plan")
	secs := int(timeout.Seconds())
	_, err = in.C.RunStream(fmt.Sprintf("timeout %d journalctl -u rnv3 -f -n 30 --no-pager -o cat", secs), in.Out, func(line string) bool {
		switch {
		case strings.Contains(line, "pass plan updated"):
			ok = true
			return true
		case strings.Contains(line, "planning failed"), strings.Contains(line, "scheduler stopped"):
			return true
		}
		return false
	})
	return ok, err
}

// Cutover runs deploy/cutover.sh: dry run first, then for real after the
// caller confirms.
func (in *Installer) CutoverDryRun() error {
	fmt.Fprintln(in.Out, "==> Cutover dry run")
	return in.runScript("./deploy/cutover.sh --dry-run")
}

func (in *Installer) Cutover(kill bool) error {
	fmt.Fprintln(in.Out, "==> Cutting over")
	args := ""
	if kill {
		args = " --kill"
	}
	return in.runScript("./deploy/cutover.sh" + args)
}

// PanelURL is where the panel ends up for a given listen address. host is
// what the operator typed for SSH: a name, an IPv4/IPv6 literal, or
// host:port / [v6]:port; only the SSH port is stripped.
func PanelURL(host, listen string) string {
	port := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		port = listen[i+1:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if port == "80" || port == "" {
		if strings.Contains(host, ":") { // bare IPv6
			return "http://[" + host + "]/"
		}
		return "http://" + host + "/"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}
