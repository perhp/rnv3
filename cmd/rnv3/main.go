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

	"github.com/perhp/rnv3/internal/capture"
	"github.com/perhp/rnv3/internal/config"
	"github.com/perhp/rnv3/internal/jobs"
	"github.com/perhp/rnv3/internal/livelog"
	"github.com/perhp/rnv3/internal/notify"
	"github.com/perhp/rnv3/internal/process"
	"github.com/perhp/rnv3/internal/publish"
	"github.com/perhp/rnv3/internal/satdumpcfg"
	"github.com/perhp/rnv3/internal/sched"
	"github.com/perhp/rnv3/internal/store"
	"github.com/perhp/rnv3/internal/tle"
	"github.com/perhp/rnv3/internal/web"
)

// version is stamped by the build script via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/rnv3/config.yaml", "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	checkOnly := flag.Bool("check", false, "validate the config and exit")
	hashPassword := flag.Bool("hash-password", false, "read a password from stdin and print its bcrypt hash for web.admin.password_hash")
	publishTest := flag.Bool("publish-test", false, "send a test station.stats event to every publish endpoint and exit")
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
	if err := run(*configPath, *checkOnly, *publishTest); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(configPath string, checkOnly, publishTest bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if checkOnly {
		fmt.Println("config OK:", configPath)
		for _, w := range cfg.Warnings() {
			fmt.Println("warning:", w)
		}
		return nil
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	slog.Info("starting rnv3", "version", version, "config", configPath)
	for _, w := range cfg.Warnings() {
		slog.Warn(w)
	}

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

	prov := config.NewProvider(cfg)
	errCh := make(chan error, 3)

	syncSatdumpCfg(prov)

	tleMgr := tle.NewManager(cfg.Paths.DataDir)
	pipeline := &process.Pipeline{Prov: prov, St: st, TLEs: tleMgr}
	live := livelog.New() // decoder output → panel terminal
	notifier := notify.New(prov)
	captureRunner := &capture.Runner{Prov: prov, St: st, Processor: pipeline, Live: live, Notify: notifier, Community: notifier}
	// Note: dry_run selects the runner at startup; toggling it requires a restart.
	var runner sched.CaptureRunner = captureRunner
	if cfg.Scheduling.DryRun {
		runner = &sched.NotImplementedRunner{St: st, DryRun: true}
	}
	publisher := publish.New(prov, st, version)
	if publishTest {
		for _, line := range publisher.Test(context.Background()) {
			fmt.Println(line)
		}
		return nil
	}
	captureRunner.Publish = publisher
	process.OnCaptureRemoved = publisher.PassDeleted
	housekeeping := &jobs.Jobs{Prov: prov, St: st, Notify: &alerter{notifier, publisher}, StateDir: cfg.Paths.DataDir}
	jobsCtx, cancelJobs := context.WithCancel(context.Background())
	defer cancelJobs()
	go housekeeping.Run(jobsCtx)
	go publisher.Run(jobsCtx)
	scheduler := sched.New(prov, st, tleMgr, runner)
	scheduler.OnPlanUpdated = publisher.SendSchedule
	schedCtx, cancelSched := context.WithCancel(context.Background())
	defer cancelSched()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		// A dead scheduler must kill the process (systemd restarts it) —
		// an HTTP server that still answers "ok" while captures silently
		// stopped is worse than a restart.
		if err := scheduler.Run(schedCtx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("scheduler stopped: %w", err)
		}
	}()

	srv, err := web.New(prov, st, tleMgr, live, scheduler, version)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
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
			// Stop the scheduler first and wait for an in-flight capture to
			// reach a terminal DB state (SatDump kill + drain can take up to
			// killGrace); fits inside systemd's default TimeoutStopSec=90.
			cancelSched()
			cancelJobs()
			select {
			case <-schedDone:
			case <-time.After(85 * time.Second):
				slog.Error("scheduler did not stop in time; passes may be left mid-state")
			}
			// Let in-flight pushes finish briefly; they are best-effort.
			pushesDone := make(chan struct{})
			go func() { captureRunner.WaitPushes(); close(pushesDone) }()
			select {
			case <-pushesDone:
			case <-time.After(3 * time.Second):
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tlsServer != nil {
				tlsServer.Shutdown(ctx)
			}
			return httpServer.Shutdown(ctx)
		case <-reload:
			fresh, err := config.Load(configPath)
			if err != nil {
				slog.Error("SIGHUP reload failed, keeping current config", "err", err)
				continue
			}
			cur := prov.Get()
			if changed := config.RestartOnlyFieldsChanged(cur, fresh); len(changed) > 0 {
				slog.Warn("restart-only settings changed in config file; keeping current values until restart",
					"settings", changed)
				fresh.Web.Listen = cur.Web.Listen
				fresh.Web.TLS = cur.Web.TLS
				fresh.Paths.DataDir = cur.Paths.DataDir
				fresh.Scheduling.DryRun = cur.Scheduling.DryRun
			}
			if !strings.EqualFold(fresh.LogLevel, cur.LogLevel) {
				slog.SetDefault(newLogger(fresh.LogLevel))
				slog.Info("log level changed", "level", fresh.LogLevel)
			}
			prov.Set(fresh)
			syncSatdumpCfg(prov)
			slog.Info("config reloaded, replanning passes")
			scheduler.Replan()
			publisher.Backfill() // a newly added endpoint gets recent history
			publisher.Kick()
		case err := <-errCh:
			return err
		}
	}
}

// syncSatdumpCfg regenerates SatDump's config from the enhancement token
// lists / map settings. Non-fatal: captures still run against whatever
// satdump_cfg.json exists (e.g. on a dev machine without SatDump).
func syncSatdumpCfg(prov *config.Provider) {
	wrote, err := satdumpcfg.Sync(prov.Get())
	switch {
	case err != nil:
		slog.Warn("cannot sync satdump_cfg.json", "err", err)
	case wrote:
		slog.Info("satdump_cfg.json regenerated", "path", prov.Get().Paths.SatdumpConfig)
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

// alerter fans watchdog alerts out to the notification channels and the
// event webhooks; daily summaries go to the channels only.
type alerter struct {
	n *notify.Notifier
	p *publish.Publisher
}

func (a *alerter) Alert(ctx context.Context, check, message string) {
	a.n.Alert(ctx, check, message)
	a.p.Alert(ctx, check, message)
}

func (a *alerter) DailySummary(ctx context.Context, annotation string, files []string) {
	a.n.DailySummary(ctx, annotation, files)
}
