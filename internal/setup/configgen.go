package setup

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/perhp/rnv3/internal/config"
)

// RenderConfig serializes cfg as the /etc/rnv3/config.yaml the daemon
// reads (strict-field compatible with config.Load).
func RenderConfig(cfg *config.Config) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# rnv3 configuration — written by rnv3-setup on %s.\n", time.Now().Format("2006-01-02 15:04"))
	b.WriteString("# Re-run rnv3-setup (Reconfigure) to change it, or edit by hand and\n")
	b.WriteString("# `sudo systemctl reload rnv3`. See config.example.yaml for every key.\n\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseConfig reads config.yaml text over the defaults (the existing
// install's config, for Reconfigure).
func ParseConfig(text string) (*config.Config, error) {
	cfg := config.Default()
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("existing config: %w", err)
	}
	return cfg, nil
}

// secretKeySuffixes: any YAML key ending in one of these is masked on
// screen (covers password_hash, auth_token, bot_token, api_token,
// smtp_password, noaa/meteor_webhook_url, link_url, url).
var secretKeySuffixes = []string{"password", "password_hash", "token", "url"}

// IsSecretKey reports whether a config key holds a credential.
func IsSecretKey(key string) bool {
	key = key[strings.LastIndex(key, ".")+1:]
	for _, s := range secretKeySuffixes {
		if strings.HasSuffix(key, s) {
			return true
		}
	}
	return false
}

// Redacted returns the YAML with secrets masked, for showing on screen.
func Redacted(yamlText []byte) string {
	var out []string
	for _, line := range strings.Split(string(yamlText), "\n") {
		t := strings.TrimSpace(line)
		if i := strings.Index(t, ":"); i > 0 && !strings.HasPrefix(t, "#") {
			key, rest := t[:i], strings.TrimSpace(t[i+1:])
			if IsSecretKey(key) && rest != "" && rest != `""` {
				indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
				line = indent + key + ": ********"
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
