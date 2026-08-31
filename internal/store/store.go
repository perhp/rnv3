// Package store owns the SQLite database: connection setup, versioned
// migrations, and typed queries. Uses modernc.org/sqlite (pure Go, no cgo) so
// the binary cross-compiles from any host.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	DB *sql.DB
}

// Open opens (creating if necessary) the database at path and applies any
// pending migrations transactionally.
func Open(path string) (*Store, error) {
	// busy_timeout guards against transient writer collisions; WAL lets the
	// web handlers read while a capture job writes (RN2's old scripts raced here).
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion() (int, error) {
	var v sql.NullInt64
	err := s.DB.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// migrate applies embedded migrations in filename order, each inside its own
// transaction, recording progress in schema_migrations. Unlike RN2's
// grep-the-schema idempotency checks, a partially failed migration rolls back.
func (s *Store) migrate() error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_ts INTEGER NOT NULL DEFAULT (strftime('%s','now'))
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		name := e.Name()
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			return fmt.Errorf("migration %q: name must start with a version number", name)
		}
		var applied int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		raw, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(raw)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			version, strings.TrimSuffix(name, ".sql")); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// Counts returns basic table counts for the status page.
func (s *Store) Counts() (passes int, images int, err error) {
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM passes`).Scan(&passes); err != nil {
		return
	}
	err = s.DB.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&images)
	return
}
