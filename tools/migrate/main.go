// migrate imports an RN2 panel.db (predict_passes / decoded_passes) and the
// capture imagery under /srv/images into the rnv3 database, then redraws
// the sky map so the stats page is complete from the first start.
//
//	migrate -old /home/pi/raspberry-noaa-v2/db/panel.db -config /etc/rnv3/config.yaml
//
// Safe to re-run: passes already imported are skipped. Stop rnv3 first so
// the two do not write the database at the same time.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/process"
	"github.com/perhp/rnv3/internal/store"
)

func main() {
	oldPath := flag.String("old", "", "path to RN2's panel.db (required)")
	configPath := flag.String("config", "/etc/rnv3/config.yaml", "rnv3 config, for the database and image paths")
	flag.Parse()
	if *oldPath == "" {
		fmt.Fprintln(os.Stderr, "usage: migrate -old /path/to/panel.db [-config /etc/rnv3/config.yaml]")
		os.Exit(2)
	}
	if err := run(*oldPath, *configPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(oldPath, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(oldPath); err != nil {
		return err
	}
	old, err := sql.Open("sqlite", "file:"+oldPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open %s: %w", oldPath, err)
	}
	defer old.Close()

	if err := os.MkdirAll(cfg.Paths.DataDir, 0o755); err != nil {
		return err
	}
	dbPath := filepath.Join(cfg.Paths.DataDir, "rnv3.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Printf("importing %s → %s\n", oldPath, dbPath)
	sum, err := Import(old, st, cfg.Paths.Images, cfg.Paths.Thumbs, time.Now(), os.Stderr)
	if err != nil {
		return err
	}
	fmt.Println(sum)

	points, err := st.SkymapPoints()
	if err != nil {
		return err
	}
	if len(points) > 0 {
		if err := process.WriteSkymap(points, cfg.Paths.Images); err != nil {
			return fmt.Errorf("sky map: %w", err)
		}
		fmt.Printf("sky map drawn from %d passes → %s\n", len(points), filepath.Join(cfg.Paths.Images, process.SkymapFilename))
	}
	return nil
}
