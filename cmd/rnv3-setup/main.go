// rnv3-setup — interactive installer that drives a Raspberry Pi over SSH:
// probe, questions, config, payload, install, optional RN2 history import
// and cutover. Runs on the operator's PC; the Pi needs nothing but sshd.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/setup"
)

// payload holds the Pi-side files; tools/release fills the directory
// before building (binaries are git-ignored, so a plain `go build` yields a
// tool that needs --payload-dir).
//
//go:embed payload
var payloadFS embed.FS

var (
	version = "dev"
	// payloadArch is the GOARCH of the embedded Pi binaries (set by
	// tools/release via -ldflags).
	payloadArch = "arm64"
)

// unameFor maps a GOARCH to what `uname -m` prints on the target.
func unameFor(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	case "arm":
		return "armv7l"
	}
	return goarch
}

func goArchFor(uname string) string {
	switch uname {
	case "aarch64":
		return "arm64"
	case "x86_64":
		return "amd64"
	case "armv7l", "armv6l":
		return "arm"
	}
	return uname
}

func main() {
	host := flag.String("host", "", "Pi hostname or IP (default: remembered)")
	user := flag.String("user", "", "SSH user (default: remembered, else pi)")
	answersPath := flag.String("answers", "", "YAML file of answers keyed by question (headless run)")
	saveAnswers := flag.String("save-answers", "", "write every answer given to this YAML file")
	payloadDir := flag.String("payload-dir", "", "directory with rnv3, rnv3-migrate, install.sh, cutover.sh, rnv3.service, config.example.yaml (overrides the embedded payload)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("rnv3-setup", version)
		return
	}
	if err := run(*host, *user, *answersPath, *saveAnswers, *payloadDir); err != nil {
		fmt.Fprintln(os.Stderr, "\nerror:", err)
		os.Exit(1)
	}
}

func run(host, user, answersPath, saveAnswers, payloadDir string) error {
	answers, err := loadAnswers(answersPath)
	if err != nil {
		return err
	}
	p := setup.NewPrompter(answers)
	// Persist answers once the whole prompt flow is over, whatever path it
	// took, so a replay reproduces every decision including the final ones.
	defer writeAnswers(p, saveAnswers)
	p.Say("rnv3-setup %s — sets up a Raspberry Pi as an rnv3 ground station over SSH.", version)
	p.Say("Press Enter to accept the value in [brackets].")
	p.Say("")

	payload, err := loadPayload(payloadDir)
	if err != nil {
		return err
	}

	// ---- connect ----------------------------------------------------------
	prof := setup.LoadProfile()
	if host == "" {
		host = p.AskRequired("pi.host", "Pi hostname or IP", prof.Host)
	}
	if user == "" {
		def := prof.User
		if def == "" {
			def = "pi"
		}
		user = p.AskRequired("pi.user", "SSH user", def)
	}
	var client *setup.Client
	var password string
	for attempt := 1; ; attempt++ {
		password = p.AskSecret("pi.password", fmt.Sprintf("Password for %s@%s", user, host), false)
		client, err = setup.Dial(host, user, password, setup.KnownHostsPath(), func(h, fp string) bool {
			p.Say("The authenticity of host %s can't be established.", h)
			p.Say("Key fingerprint: %s", fp)
			return p.AskBool("pi.trust_host_key", "Trust this host and remember its key", true)
		})
		if err == nil {
			break
		}
		if attempt >= 3 || !strings.Contains(err.Error(), "unable to authenticate") {
			return fmt.Errorf("connect to %s@%s: %w", user, host, err)
		}
		p.Say("  authentication failed, try again")
	}
	defer client.Close()
	if err := setup.SaveProfile(setup.Profile{Host: host, User: user}); err != nil {
		p.Say("  (could not remember the connection: %v)", err)
	}

	// ---- probe ------------------------------------------------------------
	p.Say("")
	p.Say("==> Connected. Looking around the Pi…")
	probe, err := setup.ProbePi(client)
	if err != nil {
		return err
	}
	p.Say("%s", probe.Summary())
	if want := unameFor(payloadArch); probe.Arch != want {
		return fmt.Errorf("the Pi reports architecture %q but this rnv3-setup carries linux/%s binaries (expects %q); build with `go run ./tools/release -arch %s`",
			probe.Arch, payloadArch, want, goArchFor(probe.Arch))
	}

	w := &setup.Wizard{P: p, Probe: probe}
	mode := w.ChooseMode()
	in := &setup.Installer{C: client, Probe: probe, Payload: payload, Password: password, Out: os.Stdout}

	switch mode {
	case setup.ModeCutover:
		return cutover(p, in, probe)
	}

	// ---- base config --------------------------------------------------------
	base := config.Default()
	switch {
	case mode == setup.ModeReconfigure && probe.RNV3Config != "":
		base, err = setup.ParseConfig(probe.RNV3Config)
		if err != nil {
			return err
		}
		p.Say("Defaults come from the current /etc/rnv3/config.yaml.")
	case probe.RN2Settings != "" && p.AskBool("prefill.rn2", "Prefill answers from raspberry-noaa-v2's settings.yml", true):
		if err := setup.PrefillFromRN2(base, probe.RN2Settings); err != nil {
			p.Say("  could not read settings.yml (%v); using rnv3 defaults", err)
		} else {
			p.Say("Defaults come from RN2's settings.yml — most questions are just Enter.")
		}
	case probe.RNV3Config != "" && p.AskBool("prefill.existing", "Start from the existing /etc/rnv3/config.yaml", true):
		base, err = setup.ParseConfig(probe.RNV3Config)
		if err != nil {
			return err
		}
	}

	cfg, err := w.Configure(base, mode)
	if err != nil {
		return err
	}
	yamlText, err := setup.RenderConfig(cfg)
	if err != nil {
		return err
	}
	if p.AskBool("show_config", "Show the generated config", false) {
		p.Say("%s", setup.Redacted(yamlText))
	}

	importHistory := false
	if mode != setup.ModeReconfigure && probe.RN2DB && probe.RNV3Config == "" {
		importHistory = p.AskBool("import.rn2_history", fmt.Sprintf("Import raspberry-noaa-v2's history (%d captures) into rnv3", probe.RN2Captures), true)
	}

	if !p.AskBool("apply", "Apply this to the Pi now", true) {
		p.Say("Nothing changed on the Pi.")
		return nil
	}

	// ---- apply --------------------------------------------------------------
	switch mode {
	case setup.ModeReconfigure:
		// A newer tool carries a newer daemon: the config it writes may use
		// settings the installed binary does not know, so upgrade first.
		if probe.RNV3Version != "rnv3 "+version {
			p.Say("==> Installed %q, this tool carries rnv3 %s — upgrading", probe.RNV3Version, version)
			if err := in.ShipPayload(); err != nil {
				return err
			}
			if err := in.WriteConfig(yamlText); err != nil {
				return err
			}
			if err := in.RunInstall(false); err != nil { // restarts the service on the new binary
				return err
			}
			break
		}
		if err := in.WriteConfig(yamlText); err != nil {
			return err
		}
		if changed := config.RestartOnlyFieldsChanged(base, cfg); len(changed) > 0 {
			p.Say("==> Restarting rnv3 (%s changed)", strings.Join(changed, ", "))
			if err := in.Restart(); err != nil {
				return err
			}
		} else {
			p.Say("==> Reloading rnv3")
			if err := in.Reload(); err != nil {
				return err
			}
		}
	default:
		if err := in.ShipPayload(); err != nil {
			return err
		}
		if err := in.WriteConfig(yamlText); err != nil {
			return err
		}
		if err := in.RunInstall(importHistory); err != nil {
			return err
		}
		if importHistory {
			if err := in.ImportHistory(); err != nil {
				p.Say("  history import failed: %v — the station still works; re-run the import later with rnv3-migrate", err)
			}
			if err := in.Start(); err != nil {
				return err
			}
		}
	}

	ok, err := in.WaitForPlan(2 * time.Minute)
	if err != nil {
		return err
	}
	p.Say("")
	if ok {
		p.Say("==> rnv3 is running and has planned its passes.")
	} else {
		p.Say("==> rnv3 is running but has not planned passes yet — check: ssh %s@%s journalctl -u rnv3 -f", user, host)
	}
	p.Say("    Panel: %s", setup.PanelURL(host, cfg.Web.Listen))
	if mode == setup.ModeSideBySide {
		p.Say("    RN2 keeps capturing. Compare the schedules, then run rnv3-setup again and choose Cut over.")
	}
	return nil
}

func cutover(p *setup.Prompter, in *setup.Installer, probe *setup.Probe) error {
	cfg, err := setup.ParseConfig(probe.RNV3Config)
	if err != nil {
		return err
	}
	if err := in.ShipPayload(); err != nil {
		return err
	}
	// cutover.sh refuses to run while dry_run is true, so the dry run is
	// shown with the guard still in place; the config is only armed after
	// the operator has confirmed.
	if cfg.Scheduling.DryRun {
		p.Say("The current config has scheduling.dry_run: true; it is switched off only after you confirm below.")
	}
	movePanel := cfg.Web.Listen == ":8080"
	if movePanel {
		p.Say("The panel is on :8080 (side-by-side); it moves to :80 once nginx is retired.")
	}
	if err := in.CutoverDryRun(); err != nil && !cfg.Scheduling.DryRun {
		return err
	}
	p.Say("")
	p.Say("This retires raspberry-noaa-v2's scheduling and web panel and hands the SDR to rnv3.")
	if strings.TrimSpace(p.Ask("cutover.confirm", "Type CUTOVER to proceed", "")) != "CUTOVER" {
		p.Say("Cutover not confirmed; nothing changed.")
		return nil
	}
	kill := false
	if probe.AtJobs > 0 {
		kill = p.AskBool("cutover.kill", "If RN2 is mid-capture, terminate it instead of waiting (up to 45 min)", false)
	}
	if cfg.Scheduling.DryRun || movePanel {
		cfg.Scheduling.DryRun = false
		if movePanel {
			cfg.Web.Listen = ":80"
		}
		yamlText, err := setup.RenderConfig(cfg)
		if err != nil {
			return err
		}
		if err := in.WriteConfig(yamlText); err != nil {
			return err
		}
	}
	if err := in.Cutover(kill); err != nil {
		return err
	}
	ok, err := in.WaitForPlan(2 * time.Minute)
	if err != nil {
		return err
	}
	p.Say("")
	if ok {
		p.Say("==> Cutover complete: rnv3 owns the SDR. Panel: %s", setup.PanelURL(in.C.Host, cfg.Web.Listen))
	} else {
		p.Say("==> Cutover ran but rnv3 has not planned passes yet — check journalctl -u rnv3 -f")
	}
	p.Say("    Watch a few passes; once happy, remove %s.", probe.RN2Home)
	return nil
}

func loadAnswers(path string) (map[string]string, error) {
	answers := map[string]string{}
	if path == "" {
		return answers, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("answers file: %w", err)
	}
	for k, v := range m {
		answers[k] = fmt.Sprint(v)
	}
	return answers, nil
}

func writeAnswers(p *setup.Prompter, path string) {
	if path == "" {
		return
	}
	used := map[string]string{}
	for k, v := range p.Used {
		if setup.IsSecretKey(k) {
			continue // never persist credentials
		}
		used[k] = v
	}
	raw, _ := yaml.Marshal(used)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		p.Say("  could not save answers: %v", err)
	} else {
		p.Say("Answers saved to %s (passwords excluded).", path)
	}
}

// loadPayload reads the Pi-side files from --payload-dir or the embedded
// copy.
func loadPayload(dir string) (setup.Payload, error) {
	read := func(name string) []byte {
		if dir != "" {
			data, _ := os.ReadFile(filepath.Join(dir, name))
			return data
		}
		data, _ := fs.ReadFile(payloadFS, "payload/"+name)
		return data
	}
	pl := setup.Payload{
		RNV3: read("rnv3"), Migrate: read("rnv3-migrate"), InstallSH: read("install.sh"), CutoverSH: read("cutover.sh"),
		Service: read("rnv3.service"), ExampleYAML: read("config.example.yaml"),
	}
	return pl, pl.Check()
}
