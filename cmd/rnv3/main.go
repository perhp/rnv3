// rnv3 — single-binary NOAA/Meteor ground station daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/web"
)

// version is stamped by the build script via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/rnv3/config.yaml", "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	checkOnly := flag.Bool("check", false, "validate the config and exit")
	hashPassword := flag.Bool("hash-password", false, "read a password from stdin and print its bcrypt hash for web.admin.password_hash")
	flag.Parse()

	if *showVersion {
		fmt.Println("rnv3", version)
		return
	}
	if *hashPassword {
		if err := runHashPassword(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*configPath, *checkOnly); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(configPath string, checkOnly bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if checkOnly {
		fmt.Println("config OK:", configPath)
		return nil
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	slog.Info("starting rnv3", "version", version, "config", configPath)

	dbPath := filepath.Join(cfg.Paths.DataDir, "rnv3.db")
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	schemaVer, _ := st.SchemaVersion()
	slog.Info("database ready", "path", dbPath, "schema_version", schemaVer)

	srv, err := web.New(cfg, st, version)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		slog.Info("http server listening", "addr", cfg.Web.Listen)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	var tlsServer *http.Server
	if cfg.Web.TLS.Enabled {
		tlsServer = &http.Server{
			Addr:              cfg.Web.TLS.Listen,
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("https server listening", "addr", cfg.Web.TLS.Listen)
			if err := tlsServer.ListenAndServeTLS(cfg.Web.TLS.CertFile, cfg.Web.TLS.KeyFile); !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)

	for {
		select {
		case sig := <-stop:
			slog.Info("shutting down", "signal", sig.String())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tlsServer != nil {
				tlsServer.Shutdown(ctx)
			}
			return httpServer.Shutdown(ctx)
		case <-reload:
			// M0: validate and log only. Later milestones swap the live config
			// into the scheduler/notifier and regenerate satdump_cfg.json.
			if fresh, err := config.Load(configPath); err != nil {
				slog.Error("SIGHUP reload failed, keeping current config", "err", err)
			} else {
				*cfg = *fresh
				slog.Info("config reloaded")
			}
		case err := <-errCh:
			return err
		}
	}
}

func runHashPassword() error {
	fmt.Fprint(os.Stderr, "password: ")
	var raw []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		_, err = fmt.Fscanln(os.Stdin, &raw)
	}
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword(raw, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
