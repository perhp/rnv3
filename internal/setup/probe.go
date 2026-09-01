package setup

import (
	"fmt"
	"strconv"
	"strings"
)

// Probe is what the tool learns about the Pi before asking anything.
type Probe struct {
	OS          string
	Arch        string
	Hostname    string
	Model       string
	MemMB       int
	Timezone    string
	SudoNoPass  bool
	SatDump     string // path, "" when missing
	SatDumpVer  string
	RTLSDRBin   string // osmocom rtl_sdr path, "" when missing
	RTLSDRUSB   string // idProduct of an attached Realtek dongle, "" when none
	NginxActive bool
	RNV3Active  bool
	RNV3Version string
	RNV3Config  string // current /etc/rnv3/config.yaml, "" when absent
	RN2Home     string // "" when RN2 is not installed
	RN2Settings string // RN2's settings.yml content, "" when absent
	RN2DB       bool
	RN2Captures int
	RN2Crons    int
	AtJobs      int
}

// probeScript prints KEY=VALUE lines; every reading is optional.
const probeScript = `
echo "os=$(. /etc/os-release 2>/dev/null; echo "$PRETTY_NAME")"
echo "arch=$(uname -m)"
echo "hostname=$(hostname)"
echo "model=$(tr -d '\0' < /proc/device-tree/model 2>/dev/null)"
echo "mem_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo 2>/dev/null)"
echo "tz=$(cat /etc/timezone 2>/dev/null)"
echo "sudo_nopasswd=$(sudo -n true 2>/dev/null && echo yes || echo no)"
echo "satdump=$(command -v satdump)"
echo "satdump_version=$(satdump --version 2>/dev/null | head -1)"
echo "rtl_sdr=$(command -v rtl_sdr)"
echo "rtlsdr_usb=$(for d in /sys/bus/usb/devices/*; do [ "$(cat "$d/idVendor" 2>/dev/null)" = 0bda ] && cat "$d/idProduct"; done 2>/dev/null | head -1)"
echo "nginx_active=$(systemctl is-active nginx 2>/dev/null)"
echo "rnv3_active=$(systemctl is-active rnv3 2>/dev/null)"
echo "rnv3_version=$(/usr/local/bin/rnv3 -version 2>/dev/null)"
RN2="$HOME/raspberry-noaa-v2"
echo "rn2_home=$([ -d "$RN2" ] && echo "$RN2")"
echo "rn2_db=$([ -f "$RN2/db/panel.db" ] && echo yes || echo no)"
echo "rn2_captures=$(sqlite3 "$RN2/db/panel.db" 'select count(*) from decoded_passes' 2>/dev/null)"
echo "rn2_crons=$(crontab -l 2>/dev/null | grep -c '^#Ansible: ')"
echo "at_jobs=$(atq 2>/dev/null | wc -l)"
`

// ProbePi runs the probe on the Pi and fetches the config files.
func ProbePi(c *Client) (*Probe, error) {
	res, err := c.Run("sh -c " + shellQuote(probeScript))
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	p := parseProbe(res.Stdout)
	if p.RN2Home != "" {
		p.RN2Settings, _, _ = c.ReadFile(p.RN2Home + "/config/settings.yml")
	}
	p.RNV3Config, _, _ = c.ReadFile("/etc/rnv3/config.yaml")
	return p, nil
}

func parseProbe(out string) *Probe {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			kv[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	return &Probe{
		OS: kv["os"], Arch: kv["arch"], Hostname: kv["hostname"], Model: kv["model"], MemMB: atoi(kv["mem_mb"]),
		Timezone: kv["tz"], SudoNoPass: kv["sudo_nopasswd"] == "yes",
		SatDump: kv["satdump"], SatDumpVer: kv["satdump_version"], RTLSDRBin: kv["rtl_sdr"], RTLSDRUSB: kv["rtlsdr_usb"],
		NginxActive: kv["nginx_active"] == "active", RNV3Active: kv["rnv3_active"] == "active", RNV3Version: kv["rnv3_version"],
		RN2Home: kv["rn2_home"], RN2DB: kv["rn2_db"] == "yes", RN2Captures: atoi(kv["rn2_captures"]),
		RN2Crons: atoi(kv["rn2_crons"]), AtJobs: atoi(kv["at_jobs"]),
	}
}

// Summary renders the probe for the operator.
func (p *Probe) Summary() string {
	var b strings.Builder
	yn := func(ok bool, yes, no string) string {
		if ok {
			return yes
		}
		return no
	}
	fmt.Fprintf(&b, "  %s, %s", p.OS, p.Arch)
	if p.Model != "" {
		fmt.Fprintf(&b, ", %s", p.Model)
	}
	if p.MemMB > 0 {
		fmt.Fprintf(&b, ", %d MB RAM", p.MemMB)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  SatDump: %s\n", yn(p.SatDump != "", p.SatDumpVer+" ("+p.SatDump+")", "missing — will be built from source (an hour or more on a Pi 4)"))
	fmt.Fprintf(&b, "  osmocom rtl-sdr: %s\n", yn(p.RTLSDRBin != "", p.RTLSDRBin, "missing — will be built from source"))
	fmt.Fprintf(&b, "  RTL-SDR on USB: %s\n", yn(p.RTLSDRUSB != "", "yes (0bda:"+p.RTLSDRUSB+")", "not detected"))
	fmt.Fprintf(&b, "  passwordless sudo: %s\n", yn(p.SudoNoPass, "yes", "no — sudo will be fed your password"))
	if p.RNV3Version != "" {
		fmt.Fprintf(&b, "  rnv3: %s installed, service %s\n", p.RNV3Version, yn(p.RNV3Active, "running", "stopped"))
	} else {
		b.WriteString("  rnv3: not installed\n")
	}
	if p.RN2Home != "" {
		fmt.Fprintf(&b, "  raspberry-noaa-v2: %s (settings.yml %s, panel.db %s", p.RN2Home,
			yn(p.RN2Settings != "", "found", "missing"), yn(p.RN2DB, fmt.Sprintf("with %d captures", p.RN2Captures), "missing"))
		fmt.Fprintf(&b, ", %d cron entries, %d at jobs, nginx %s)\n", p.RN2Crons, p.AtJobs, yn(p.NginxActive, "active", "inactive"))
	} else {
		b.WriteString("  raspberry-noaa-v2: not found\n")
	}
	return b.String()
}
