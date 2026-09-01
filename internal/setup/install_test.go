package setup

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testPayload() Payload {
	return Payload{RNV3: []byte("ELF-rnv3"), Migrate: []byte("ELF-migrate"), InstallSH: []byte("#!/bin/sh\n"),
		CutoverSH: []byte("#!/bin/sh\n"), Service: []byte("[Unit]\n"), ExampleYAML: []byte("station: {}\n")}
}

// installHandlers emulate the remote side of the passwordless-sudo install
// path: sudo -n commands, the nohup launcher, the log tail, the migrator
// and the journal wait.
func installHandlers(pi *fakePi, installExit int, planLine string) {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	pi.handlers = append(pi.handlers, func(cmd string, ch ssh.Channel) (bool, int) {
		switch {
		case strings.HasPrefix(cmd, "rm -rf /tmp/rnv3-deploy"):
			return true, 0
		case strings.HasPrefix(cmd, "sudo -n "):
			return true, 0
		case strings.Contains(cmd, "nohup sh -c"):
			// Record what would run, hand back a pid.
			pi.files["/tmp/rnv3-deploy/run.log"] = []byte(fmt.Sprintf("==> pretending to install\n%s%d\n", exitMarker, installExit))
			fmt.Fprintln(ch, "4242")
			return true, 0
		case strings.HasPrefix(cmd, "tail -n +1 --pid=4242 -f /tmp/rnv3-deploy/run.log"):
			ch.Write(pi.files["/tmp/rnv3-deploy/run.log"])
			time.Sleep(2 * time.Second) // a real tail keeps following; the client must stop on the marker
			return true, 0
		case strings.HasPrefix(cmd, "/usr/local/bin/rnv3-migrate "):
			fmt.Fprintln(ch, "passes: 10 seen, 10 inserted")
			return true, 0
		case strings.HasPrefix(cmd, "timeout 120 journalctl"):
			fmt.Fprintln(ch, "starting rnv3")
			if planLine != "" {
				fmt.Fprintln(ch, planLine)
			}
			time.Sleep(2 * time.Second)
			return true, 0
		}
		return false, 0
	})
}

func TestInstallerFullFlowPasswordlessSudo(t *testing.T) {
	pi := startFakePi(t)
	installHandlers(pi, 0, `msg="pass plan updated" scheduled=12 skipped=3`)
	c := dialFake(t, pi)
	var out bytes.Buffer
	in := &Installer{C: c, Probe: &Probe{SudoNoPass: true, SatDump: "/usr/bin/satdump", RTLSDRBin: "/usr/local/bin/rtl_sdr",
		RN2Home: "/home/pi/raspberry-noaa-v2"}, Payload: testPayload(), Password: "raspberry", Out: &out}

	if err := in.ShipPayload(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"/tmp/rnv3-deploy/rnv3", "/tmp/rnv3-deploy/rnv3-migrate", "/tmp/rnv3-deploy/deploy/install.sh",
		"/tmp/rnv3-deploy/deploy/cutover.sh", "/tmp/rnv3-deploy/deploy/rnv3.service", "/tmp/rnv3-deploy/config.example.yaml"} {
		if _, ok := pi.files[f]; !ok {
			t.Errorf("%s not uploaded", f)
		}
	}
	if err := in.WriteConfig([]byte("station:\n  name: x\n")); err != nil {
		t.Fatal(err)
	}
	if !contains(pi.log, "sudo -n sh -c 'mkdir -p /etc/rnv3 && install -m 0600 -o pi -g pi /tmp/rnv3-deploy/config.yaml /etc/rnv3/config.yaml'") {
		t.Errorf("config not installed with sudo; commands: %v", pi.log)
	}

	start := time.Now()
	if err := in.RunInstall(true); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Error("client did not stop tailing at the exit marker")
	}
	launched := ""
	for _, cmd := range pi.log {
		if strings.Contains(cmd, "nohup sh -c") {
			launched = cmd
		}
	}
	if !strings.Contains(launched, "./deploy/install.sh --no-start ./rnv3") || !strings.Contains(launched, "< /dev/null &") {
		t.Errorf("install launched wrong: %s", launched)
	}
	if !strings.Contains(out.String(), "pretending to install") {
		t.Error("install output not streamed")
	}

	if err := in.ImportHistory(); err != nil {
		t.Fatal(err)
	}
	if !contains(pi.log, "sudo -n systemctl stop rnv3") || !strings.Contains(out.String(), "10 inserted") {
		t.Errorf("import sequence wrong: %v", pi.log)
	}
	if err := in.Start(); err != nil {
		t.Fatal(err)
	}
	ok, err := in.WaitForPlan(2 * time.Minute)
	if err != nil || !ok {
		t.Errorf("wait for plan: ok=%v err=%v", ok, err)
	}
}

func TestInstallerReportsScriptFailure(t *testing.T) {
	pi := startFakePi(t)
	installHandlers(pi, 7, "")
	c := dialFake(t, pi)
	in := &Installer{C: c, Probe: &Probe{SudoNoPass: true}, Payload: testPayload(), Password: "x", Out: io.Discard}
	err := in.RunInstall(false)
	if err == nil || !strings.Contains(err.Error(), "status 7") {
		t.Errorf("failure not reported: %v", err)
	}
	ok, err := in.WaitForPlan(time.Minute)
	if err != nil || ok {
		t.Errorf("no plan line must yield ok=false: ok=%v err=%v", ok, err)
	}
}

func TestInstallerPTYPathFeedsSudoPassword(t *testing.T) {
	pi := startFakePi(t)
	pi.mu.Lock()
	pi.handlers = append(pi.handlers, func(cmd string, ch ssh.Channel) (bool, int) {
		if !strings.HasPrefix(cmd, "sudo -S -p 'RNV3_SUDO_PROMPT' -v") {
			return false, 0
		}
		fmt.Fprint(ch, "RNV3_SUDO_PROMPT")
		line := make([]byte, 64)
		n, _ := ch.Read(line)
		if strings.TrimSpace(string(line[:n])) != pi.password {
			fmt.Fprintln(ch, "Sorry, try again.")
			return true, 1
		}
		fmt.Fprintln(ch, "\n==> install running under pty")
		fmt.Fprintln(ch, exitMarker+"0")
		return true, 0
	})
	pi.mu.Unlock()
	c := dialFake(t, pi)
	var out bytes.Buffer
	in := &Installer{C: c, Probe: &Probe{SudoNoPass: false}, Payload: testPayload(), Password: "raspberry", Out: &out}
	if err := in.RunInstall(false); err != nil {
		t.Fatalf("pty install: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "running under pty") {
		t.Error("pty output not streamed")
	}
	in.Password = "wrong"
	if err := in.RunInstall(false); err == nil {
		t.Error("wrong sudo password must fail the install")
	}
}

func TestPayloadCheck(t *testing.T) {
	p := testPayload()
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	p.RNV3 = nil
	p.CutoverSH = nil
	err := p.Check()
	if err == nil || !strings.Contains(err.Error(), "rnv3") || !strings.Contains(err.Error(), "cutover.sh") || !strings.Contains(err.Error(), "--payload-dir") {
		t.Errorf("incomplete payload: %v", err)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
