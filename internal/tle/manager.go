package tle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultURLTemplate fetches one catalog entry from Celestrak, same source
// RN2 used (%d is the NORAD id).
const DefaultURLTemplate = "https://celestrak.org/NORAD/elements/gp.php?CATNR=%d&FORMAT=TLE"

// Manager caches the station's TLE set on disk and refreshes it from
// Celestrak. Refresh is all-or-nothing: the cached file is only replaced when
// every requested satellite validated (RN2's schedule.sh behaved the same way
// so bad orbital data never overwrites known-good data).
type Manager struct {
	dir         string
	urlTemplate string
	client      *http.Client
}

func NewManager(dataDir string) *Manager {
	return &Manager{
		dir:         filepath.Join(dataDir, "tle"),
		urlTemplate: DefaultURLTemplate,
		client:      &http.Client{Timeout: 60 * time.Second},
	}
}

func (m *Manager) path() string { return filepath.Join(m.dir, "current.tle") }

// Load reads the cached set and its fetch time. os.IsNotExist(err) signals
// a first run with no cache yet.
func (m *Manager) Load() (Set, time.Time, error) {
	f, err := os.Open(m.path())
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	set, err := ParseSet(f)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("cached TLE file %s is corrupt: %w", m.path(), err)
	}
	return set, info.ModTime(), nil
}

// Age returns how old the cached set is; a very large duration when absent.
func (m *Manager) Age() time.Duration {
	info, err := os.Stat(m.path())
	if err != nil {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(info.ModTime())
}

// Refresh fetches all ids, validates each, and atomically replaces the cache
// (keeping the previous file as current.tle.bak). On any failure the old
// cache is untouched and the error is returned.
func (m *Manager) Refresh(ctx context.Context, ids []int) (Set, error) {
	set := Set{}
	for _, id := range ids {
		t, err := m.fetchOne(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("fetch NORAD %d: %w", id, err)
		}
		set[id] = t
	}

	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return nil, err
	}
	tmp := m.path() + ".tmp"
	if err := os.WriteFile(tmp, []byte(set.Format()), 0o644); err != nil {
		return nil, err
	}
	if _, err := os.Stat(m.path()); err == nil {
		if err := os.Rename(m.path(), m.path()+".bak"); err != nil {
			os.Remove(tmp)
			return nil, err
		}
	}
	if err := os.Rename(tmp, m.path()); err != nil {
		return nil, err
	}
	slog.Info("TLE cache refreshed", "satellites", len(set), "path", m.path())
	return set, nil
}

func (m *Manager) fetchOne(ctx context.Context, id int) (TLE, error) {
	url := fmt.Sprintf(m.urlTemplate, id)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return TLE{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		t, err := m.fetchOnce(ctx, url, id)
		if err == nil {
			return t, nil
		}
		lastErr = err
		slog.Warn("TLE fetch attempt failed", "norad_id", id, "attempt", attempt, "err", err)
	}
	return TLE{}, lastErr
}

func (m *Manager) fetchOnce(ctx context.Context, url string, id int) (TLE, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return TLE{}, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return TLE{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TLE{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return TLE{}, err
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	if strings.Contains(text, "No GP data found") {
		return TLE{}, fmt.Errorf("celestrak has no GP data for this id")
	}
	var name, line1, line2 string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " ")
		switch {
		case strings.HasPrefix(line, "1 "):
			line1 = line
		case strings.HasPrefix(line, "2 "):
			line2 = line
		case line != "" && name == "":
			name = strings.TrimSpace(line)
		}
	}
	if line1 == "" || line2 == "" {
		return TLE{}, fmt.Errorf("response did not contain a TLE pair")
	}
	if err := ValidateLines(line1, line2, id); err != nil {
		return TLE{}, err
	}
	return TLE{Name: name, NoradID: id, Line1: line1, Line2: line2}, nil
}
