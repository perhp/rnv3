# rnv3 — Rewrite plan for raspberry-noaa-v2

A ground-up rewrite of [raspberry-noaa-v2](https://github.com/perhp/raspberry-noaa-v2) (RN2) as a
**single Go binary** running as one systemd service on Debian Trixie (Raspberry Pi 3/4/5 or x64).
It replaces bash + Python + PHP + Ansible + nginx + php-fpm + cron + `at` + sudoers with one process.
The only remaining external runtime dependencies: **SatDump** (the decoder) and the SDR driver stack
(udev rules, osmocom rtl-sdr for V4 support).

Scope decisions (agreed 2026-08-31):

- **Platform:** Pi/Debian Linux only (same target as RN2).
- **Architecture:** single-binary daemon.
- **Language:** Go.
- **Feature scope:** core + web UI; trim rarely-used push processors (keep/cut list below).
- **Push channels kept:** generic webhook, Discord, Telegram, Pushover, email.
- **Data migration:** one-time importer for the existing `panel.db` + `/srv/images`.
- **Web UI:** port the current ops-console/terminal design 1:1 (no redesign).
- **Repo:** this new repo; the old repo stays as the running reference until rnv3 is validated on the Pi.

Why Go: static single binary cross-compiled from Windows in one command
(`GOOS=linux GOARCH=arm64 go build`) and deployed by copying one file over SSH — no venv, no
composer, no apt churn on the Pi per deploy. Goroutines map exactly onto the domain (scheduler loop,
capture jobs, HTTP server, watchdog ticker). `embed.FS` bakes the entire web UI into the binary.
Mature libs exist for every need (SGP4, SQLite, image handling).

---

## Part 1 — What raspberry-noaa-v2 is today (investigation summary)

### The stack (7 technologies for one product)

Bash (~15 runtime scripts) + Python (6 tools) + PHP 8.4 webpanel (hand-rolled MVC + Twig) + Ansible
(2 roles, ~30 task files) + nginx/php-fpm + cron + `at`, with SQLite as the store. The real signal
chain is short — **SatDump does all the actual radio/decode work** — everything else is
orchestration, glue, and presentation.

### Feature inventory

**Scheduling & prediction**
- Nightly cron runs `schedule.sh -t`: downloads TLEs from Celestrak (validated, atomically
  installed, backed up; scheduling aborts on bad TLEs), predicts passes 7 days out with
  python3-ephem, creates one `at` job per pass, inserts rows in `predict_passes` (SQLite).
- Filters: per-satellite min elevation, Meteor-only sun-elevation gate at schedule time.
- Overlap resolution: `select_best_overlapping_passes.py` compares adjacent pairs
  (Meteor-over-NOAA preference, else higher elevation), cancels the loser by scraping `atq` output
  with locale-dependent timestamp matching.

**Capture & decode** (`receive_noaa.sh` / `receive_meteor.sh`, run by `at`)
- 5 satellites (NOAA 15/18/19 APT, Meteor-M2 3/4 LRPT; frequencies hardcoded in `common.sh`),
  7 SDR types (rtlsdr / airspy_mini / airspy_r2 / airspy_hf_plus_discovery / hackrf / sdrplay /
  mirisdr) with per-type samplerate and gain-flag mapping; bias tee, PPM correction (RTL-only),
  multi-dongle `--source_id`.
- RAM-vs-SD buffering decision based on free memory; `/var/ramfs` tmpfs (1000M).
- SatDump `live` with `--finish_processing`; Meteor gets a `timeout` watchdog and 80k-interleaving
  pipeline selection; NOAA gets wav save + sox resample to 11025 Hz.
- Post-processing: file renaming/prefix-stripping, Northbound 180° flip,
  calibrated-over-uncalibrated dedup, JPEG normalization + 300px thumbnails (ImageMagick),
  day/night classification via sun elevation, SNR extraction from SatDump logs, CADU frame-loss
  stats (Meteor), polar az/el + direction plots (matplotlib), status state machine
  (`capturing → processing → received/failed`) with error text and an EXIT trap.

**Recurring jobs (cron)**
- Daily imagery: per-suffix animated-GIF timelapses + brightest-pixel mosaics (works without
  warping because SatDump projection grids are fixed and station-anchored), SNR/daylight filters,
  flock-guarded, atomic writes.
- Sky-quality polar map of all historical passes (colored by SNR, failures marked).
- Best-of-day summary push (ranked by SNR then elevation, representative-enhancement candidate lists).
- Hourly health watchdog: 5 checks (no captures in N hours, all-failing, disk usage, empty `at`
  queue, RTL-SDR USB presence), deduped to one alert per check per 24 h.
- Audio retention/pruning, DB backups.

**Webpanel (PHP)** — the recently built ops-console design:
- Live terminal widget with log tail / fullscreen / smart scroll-follow; idle dashboard (clocks,
  TLE age with warn thresholds, vitals: CPU temp / load / mem / disk / uptime, upcoming queue).
- Client-rendered schedule table with status lamps (done/failed/running/processing/conflict/next/
  missed) and elevation / frame-yield gauges; clock-offset interpolation between 5 s polls.
- Filterable capture gallery (sat / day-night / min elevation, pagination); per-pass enhancement
  viewer (filesystem-glob driven); pure-CSS stats page (chart recorder, per-sat table,
  mosaics/timelapses with download buttons, sky map).
- Admin delete-pass / delete-capture with `<dialog>` confirms, optional HTTP Basic lock.
- JSON API (`/api/passes`, `/api/captures`, `/api/capture`, `/api/status`, `/api/rss`) + the rich
  `/passes/status` endpoint (vitals, log tail, TLE age).
- 17 languages (16 stale — only ~34/87 keys translated); satvis / solar-terminator / coronal
  instrument embeds (config-gated, cache-busted).

**Push processors (12)**: Discord, Telegram, Pushover, email, generic webhook, Slack, Matrix,
Twitter/X, Bluesky, Mastodon, Facebook, Instagram — plus a quality gate (min elevation/SNR) that
mutes social pushes but deliberately not the webhook.

**Config/install**: ~115 settings in `settings.yml` → JSON-schema validation → Ansible renders 4
artifacts (`~/.noaa-v2.conf` env file sourced by every script, a 5,308-line SatDump config with
per-composite `autogen` flags derived from enhancement token lists, PHP `Config.php`, nginx vhosts)
plus cron entries, udev rules, tmpfs mount, sudoers (`www-data → atrm`), ntpsec, logrotate; SatDump
1.2.2 and osmocom rtl-sdr built from source; verification tool with a 205-entry permissions manifest.

### Defects found during investigation (fix structurally in the rewrite)

1. **NOAA passes never get `-website-thumbnail.jpg`** — only Meteor creates it, but the gallery,
   admin view, and RSS enclosure reference it unconditionally → broken NOAA gallery cards.
2. `prune_oldest.sh` has an inverted `-mtime` sign — it deletes the **newest** files.
3. `push_bluesky.py` will `IndexError` on <3 images and posts duplicates otherwise.
4. Meteor's DB insert lacks the id-reselect NOAA has → reruns create duplicate `decoded_passes` rows.
5. Thermal-gauge overlay is dead code (`ENHANCEMENT` is never set anymore) though the setting
   defaults to true.
6. Ansible `ramfs.yml` creates a literal directory named `ramfs_path` (unquoted var) — enshrined in
   the verification tool's permissions manifest.
7. The TLS nginx template uses `ssl on;`, removed in nginx ≥1.15 → hard config error on Trixie.
8. Admin auth: plaintext credentials in a world-readable file, non-constant-time compare, deletions
   via GET, `atrm` via sudo shell interpolation; `/passes/status` exposes host vitals + logs
   unauthenticated.
9. SatDump's decode deliberately writes to the `at`-job's **cwd** via an intentionally-undefined
   variable — the whole decode step is cwd-dependent.
10. NOAA capture has no watchdog timeout (Meteor does); older sqlite call sites lack `.timeout` and
    can collide with webpanel reads.

---

## Part 2 — The rewrite

### Architecture

```
rnv3 (single process, systemd unit)
├── config loader        config.yaml → typed struct, validated at startup, SIGHUP reload
├── store                SQLite (modernc.org/sqlite, pure-Go: no cgo, trivial cross-compile)
├── predictor            SGP4 + solar position — replaces ephem/pass_predict.py/sun.py
├── scheduler            in-process: recomputes pass plan on boot/TLE refresh/config change,
│                        resolves overlaps globally, fires captures via time.Timer
│                        → replaces cron + at + atq scraping + sudoers/atrm entirely
├── capture runner       exec's satdump live in a per-pass work dir (os/exec, context timeout),
│                        streams stdout for SNR/progress, state machine in DB
├── processing           pure-Go: rename/flip/dedup, JPEG encode, thumbnails, polar plots (SVG),
│                        sky map, daily mosaics (lighten-blend) + GIF timelapses
│                        → replaces ImageMagick, matplotlib, sox
├── notifier             webhook / Discord / Telegram / Pushover / email (net/smtp) + quality gate
├── watchdog             in-process ticker: same 5 health checks, deduped alerts
├── retention            unified pruning (images + thumbs + audio + DB rows, correct this time)
└── http server          net/http: embedded UI (ported ops-console), JSON API, RSS,
                         SSE for the live terminal (replaces 5 s polling), optional TLS
```

### Key design decisions

- **Scheduler owns everything in-process.** Pass plan lives in the DB; on every boot the daemon
  recomputes from current TLEs and config. Canceling a pass from the UI is a DB update — no
  `at_job_id`, no `atq` parsing, no sudo. Overlap resolution becomes a proper interval-scheduling
  pass over the whole window (fixes the adjacent-pairs-only weakness).
- **One `passes` table with a state machine**
  (`scheduled → capturing → processing → decoded | failed | skipped`) replaces the
  `predict_passes` / `decoded_passes` split, plus an `images` table (pass_id, kind, path) replacing
  filesystem globbing in the UI. Frame stats, SNR, gain, error text all live on the pass row.
- **Per-pass work directory** (`/srv/work/<pass-id>/`) for SatDump output — kills the
  cwd-dependence hack. Every capture (NOAA included) runs under a context deadline.
- **Config is read live** by the daemon — no Ansible templating layer. The 5,308-line SatDump
  config becomes a Go-side generator: the daemon writes `satdump_cfg.json` from the enhancement
  token lists at startup (same tokens, same semantics).
- **Satellite definitions (frequencies, NORAD ids, pipelines) live in config** with defaults for
  the current 5 birds — no more hardcoding in `common.sh`.
- **Web UI is a port, not a redesign**: `rn2.css` / `rn2.js` carry over nearly verbatim; Twig
  templates become `html/template`; the client-rendered schedule table keeps its JS. Upgrade: the
  `/passes/status` poll becomes Server-Sent Events, so the terminal streams SatDump output live.
  Admin: bcrypt-hashed password, session cookie, POST + CSRF for deletes. i18n: English only,
  strings in one map so translations can return later.
- **Audio artifacts**: keep `--save_wav`; store as-is or resample in Go (sox dropped). CADU
  frame-stats parser reimplemented in Go (~100 lines).
- **Polar plots & sky map as server-side SVG** — sharper than matplotlib PNGs, zero dependencies,
  themeable with the ops-console palette.

### Kept / cut

**Kept (full parity):** all 5 satellites; all 7 SDR types + bias tee / PPM / device-string; TLE
pipeline with validation/atomicity; min-elevation + sun gates; Meteor-over-NOAA preference; ramfs
threshold logic; day/night classification; enhancement token config; map overlays; flips/crops;
thumbnails; polar plots; sky-quality map; daily mosaics + timelapses; best-of-day push; quality
gate; health watchdog; retention/pruning; the entire web UI including instruments
(satvis/terminator/coronal); JSON API; RSS; admin; community-composites upload (one HTTP POST).

**Cut:** Twitter/X, Bluesky, Mastodon, Facebook, Instagram, Slack, Matrix pushers (the webhook
covers integrators; these were also the buggiest code); Instagram composite generation; thermal
overlay (already dead); wxtoimg vestige columns; 16 stale translations; Ansible / nginx / PHP /
`at` / cron / sudoers as technologies.

### Repo layout

```
cmd/rnv3/            main
internal/config/     schema, defaults, validation, SIGHUP
internal/store/      sqlite, migrations (versioned, transactional), old-panel.db importer
internal/predict/    SGP4, sun position, pass computation
internal/sched/      plan builder, overlap resolver, timers
internal/capture/    satdump runner, per-SDR profiles, SNR/log parsing, cadu stats
internal/process/    image pipeline, polar/skymap SVG, mosaics, timelapse
internal/notify/     webhook, discord, telegram, pushover, email, quality gate
internal/watchdog/
internal/web/        handlers, SSE, embedded ui/ (templates, rn2.css, rn2.js)
deploy/              systemd unit, udev rules, install.sh (~150 lines: apt deps,
                     SatDump + osmocom rtl-sdr build if missing, dirs, tmpfs, service)
tools/migrate/       one-shot importer: old panel.db + /srv/images → new schema
```

The installer shrinks from Ansible-plus-verification-manifest to a small idempotent shell script:
binary, unit file, udev rules, directories, SatDump.

### Milestones

- [x] **M0 — Skeleton**: repo, config loader with current `settings.yml` values mapped over,
  SQLite store + migrations, systemd unit, cross-compile + deploy script to the Pi.
  *Exit: daemon runs on raspinoaa, serves a hello/status page.*
  (Code complete + local smoke test 2026-08-31; on-Pi run deferred by choice.)
- [ ] **M1 — Predict & schedule**: TLE fetch/validate, SGP4 passes, sun gates, overlap resolver,
  plan visible in DB. *Exit: predicted passes match the current system's schedule within seconds —
  run both side by side (rnv3 in dry-run mode, no SDR contention).*
  (Code complete 2026-08-31: live Celestrak fetch + 7-day plan verified locally; the
  side-by-side comparison against RN2 happens when we first deploy to the Pi.)
- [ ] **M2 — Capture & decode**: SatDump runner with per-SDR profiles, work dirs, state machine,
  SNR/frame stats, ramfs logic. *Exit: a real NOAA and a real Meteor pass decoded end-to-end by
  rnv3 (old system paused during the test).*
- [ ] **M3 — Processing**: rename/flip/dedup rules ported exactly (they encode hard-won
  SatDump-output knowledge), thumbnails, polar SVGs, sky map, mosaics/timelapses.
  *Exit: artifacts visually match current output for the same pass.*
- [ ] **M4 — Web UI port**: templates + API + SSE terminal + admin + RSS; the migration importer,
  so the UI is tested against real history. *Exit: feature-parity walkthrough against the PHP panel.*
- [ ] **M5 — Notifications, watchdog, retention, best-of-day.**
- [ ] **M6 — Cutover**: install script hardening, disable old cron/at jobs, run rnv3 as the sole
  owner of the SDR for a week, then retire the old stack on the Pi.

Development stays on Windows; every milestone validates on the Pi ("raspinoaa") — the deploy
artifact is one binary, so the exec-bit / git-pull pain of the old flow disappears.

### Risks

- **SatDump coupling** is the big one: output filenames/flags are the contract, and the M3 rename
  rules encode current 1.2.2 behavior. Mitigation: pin the SatDump version in the installer, keep
  the rename rules table-driven and covered by golden-file tests using real pass output captured
  from the Pi.
- **Prediction parity**: SGP4 vs ephem will differ by seconds; M1's side-by-side comparison catches
  anything material.
- **Pi-only validation**: nothing here runs on Windows; compensations are Go's unit-testability
  (predictor, overlap resolver, cadu parser, rename rules all test off-target) and dry-run mode.

Rough outcome: ~7 technologies → 1 binary + SatDump; the ~115-setting surface survives as one
`config.yaml`; every defect in the list above is fixed structurally rather than patched.
