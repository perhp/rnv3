package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Profile remembers how to reach the Pi — never the password.
type Profile struct {
	Host string `json:"host"`
	User string `json:"user"`
}

// ConfigDir is %APPDATA%\rnv3 (or the platform equivalent).
func ConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base, _ = os.UserHomeDir()
	}
	return filepath.Join(base, "rnv3")
}

func profilePath() string    { return filepath.Join(ConfigDir(), "setup.json") }
func KnownHostsPath() string { return filepath.Join(ConfigDir(), "known_hosts") }

func LoadProfile() Profile {
	var p Profile
	raw, err := os.ReadFile(profilePath())
	if err == nil {
		json.Unmarshal(raw, &p)
	}
	return p
}

func SaveProfile(p Profile) error {
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(profilePath(), raw, 0o600)
}
