package config

import "sync/atomic"

// Provider hands out immutable config snapshots and lets a SIGHUP reload swap
// in a new one atomically. Every reader takes a snapshot per unit of work
// (per HTTP request, per scheduler iteration, per capture) — never hold one
// across reloads, and never mutate one.
type Provider struct {
	p atomic.Pointer[Config]
}

func NewProvider(c *Config) *Provider {
	pr := &Provider{}
	pr.p.Store(c)
	return pr
}

// Get returns the current snapshot. Treat it as read-only.
func (pr *Provider) Get() *Config { return pr.p.Load() }

// Set swaps in a new snapshot for subsequent Get calls.
func (pr *Provider) Set(c *Config) { pr.p.Store(c) }

// RestartOnlyFieldsChanged reports which settings cannot take effect via
// reload; the caller keeps the old values and warns.
func RestartOnlyFieldsChanged(old, fresh *Config) []string {
	var changed []string
	if old.Web.Listen != fresh.Web.Listen {
		changed = append(changed, "web.listen")
	}
	if old.Web.TLS != fresh.Web.TLS {
		changed = append(changed, "web.tls")
	}
	if old.Paths.DataDir != fresh.Paths.DataDir {
		changed = append(changed, "paths.data_dir")
	}
	return changed
}
