# rnv3

**A NOAA APT / Meteor-M LRPT weather-satellite ground station in one binary.**

rnv3 is the rewrite of [raspberry-noaa-v2](https://github.com/perhp/raspberry-noaa-v2) (RN2).
Where RN2 was bash + Python + PHP + Ansible + nginx + cron + `at`, rnv3 is a single Go daemon
that does everything itself:

- predicts passes (SGP4), schedules and captures them with [SatDump](https://github.com/SatDump/SatDump)
- produces the enhancements, thumbnails, polar plots, sky-quality map, daily mosaics and timelapses
- serves the web panel (live terminal, schedule, gallery, stats, admin) on port 80
- pushes captures to a webhook, Discord, Telegram, Pushover or email
- streams every pass, image, schedule and health sample to your own website as events
- watches its own health, prunes old captures, keeps its TLEs fresh

It runs on 64-bit Raspberry Pi OS / Debian 13 (Trixie) on a Pi 3/4/5, and you set it up from
your PC without ever opening a terminal on the Pi.

---

## Quick start

Everything happens on your PC; the Pi only needs Raspberry Pi OS with SSH enabled (see
[What you need](#1-what-you-need)). Skip the install lines for tools you already have —
`git --version` and `go version` tell you.

**Windows** (PowerShell)

```powershell
winget install --id Git.Git -e          # step 1: Git
winget install --id GoLang.Go -e        # step 2: Go
# step 3: close and reopen the terminal so PATH picks up the new tools
git clone https://github.com/perhp/rnv3.git   # step 4: get rnv3
cd rnv3
.\deploy\release.ps1                    # step 5: build the setup tool → dist\rnv3-setup.exe
.\dist\rnv3-setup.exe                   # step 6: run it — it installs rnv3 on the Pi
```

**macOS** (Terminal)

```bash
xcode-select --install                  # step 1: Git (Apple's command line tools)
brew install go                         # step 2: Go — or the .pkg from https://go.dev/dl/
git clone https://github.com/perhp/rnv3.git   # step 3: get rnv3
cd rnv3
./deploy/release.sh                     # step 4: build the setup tool → dist/rnv3-setup
./dist/rnv3-setup                       # step 5: run it — it installs rnv3 on the Pi
```

**Linux** (Debian / Ubuntu; other distros: the same packages from your package manager)

```bash
sudo apt install -y git golang-go       # step 1: Git and Go (an older Go fetches the right one itself)
git clone https://github.com/perhp/rnv3.git   # step 2: get rnv3
cd rnv3
./deploy/release.sh                     # step 3: build the setup tool → dist/rnv3-setup
./dist/rnv3-setup                       # step 4: run it — it installs rnv3 on the Pi
```

The setup tool asks for the Pi's address and password, then a few questions about your station,
and ends by printing the panel address. Details, migration from raspberry-noaa-v2 and everything
else below.

---

## Contents

0. [Quick start](#quick-start)
1. [What you need](#1-what-you-need)
2. [Install on a new Pi](#2-install-on-a-new-pi)
3. [Migrate from raspberry-noaa-v2](#3-migrate-from-raspberry-noaa-v2)
4. [Using the station](#4-using-the-station)
5. [Changing settings](#5-changing-settings)
6. [Upgrading rnv3](#6-upgrading-rnv3)
7. [Publishing to your own website](#7-publishing-to-your-own-website)
8. [Where things live on the Pi](#8-where-things-live-on-the-pi)
9. [Troubleshooting](#9-troubleshooting)
10. [Developing rnv3](#10-developing-rnv3)
11. [Appendix: what the installer sets up](#11-appendix-what-the-installer-sets-up)

---

## 1. What you need

**Hardware**

- Raspberry Pi 3, 4 or 5 (4 GB RAM recommended) with a 64-bit OS
- an SDR: RTL-SDR (v3 or v4), Airspy Mini / R2 / HF+ Discovery, HackRF, SDRplay or MiriSDR
- a 137 MHz antenna (QFH, turnstile or V-dipole) — same as for RN2

**On the Pi**

- Raspberry Pi OS Lite (64-bit, Debian 13 "Trixie") or plain Debian 13
- SSH enabled, and you know the user name and password (Raspberry Pi Imager lets you set both)
- the Pi on your network, reachable by name (e.g. `raspinoaa`) or IP

**On your PC** (Windows, macOS or Linux — the build tool is the same Go program on all three)

- [Go](https://go.dev/dl/) 1.27 or newer, to build the tools once (an older 1.21+ Go fetches it
  automatically; the wrapper scripts find Go in its usual install folder even if it is not on
  your PATH)
- Git, to clone this repository

That's all. SatDump and the SDR drivers are installed on the Pi by rnv3 itself.

---

## 2. Install on a new Pi

### Step 1 — build the setup tool

```powershell
git clone https://github.com/perhp/rnv3.git
cd rnv3
.\deploy\release.ps1          # Windows
./deploy/release.sh           # macOS / Linux
```

Both are thin wrappers around `go run ./tools/release` (they only locate Go), which
cross-compiles the Pi binaries and packs them into **`dist\rnv3-setup.exe`** (Windows) or
**`dist/rnv3-setup`** (macOS / Linux) — a self-contained installer for your PC. You only
rebuild it when you update rnv3. Options, same on every OS (PowerShell also accepts them
capitalised, e.g. `-Arch`):

| Option | Use |
|---|---|
| `-target darwin/arm64` | build the installer for *another* PC — here a Mac; output `dist/rnv3-setup-darwin-arm64` |
| `-arch amd64` | the station is an x64 PC running Debian instead of a Pi (default `arm64`) |

### Step 2 — run it

```powershell
.\dist\rnv3-setup.exe         # Windows
./dist/rnv3-setup             # macOS / Linux
```

(The rest of this guide shows the Windows form; on macOS / Linux substitute `./dist/rnv3-setup`.)

The tool asks a series of questions. Menus are driven with the **arrow keys** (Space toggles
items in a list, Enter confirms, y/n or ←/→ for yes/no); for typed values, press **Enter** to
accept the default shown in `[brackets]`.

1. **Connect** — Pi hostname, SSH user, password. The first time it shows the Pi's SSH key
   fingerprint and asks you to trust it (it remembers the host and user for next time, never
   the password).
2. **Probe** — it looks around and tells you what it found: OS, model, memory, whether SatDump
   and the RTL-SDR driver are already installed, whether an SDR is plugged in, and whether an
   old raspberry-noaa-v2 install exists.
3. **Mode** — on a fresh Pi choose *Install rnv3 as the station*.
4. **Station** — hostname, a free-text location for notifications, latitude, longitude,
   altitude.
5. **SDR** — pick your receiver type.
6. **Satellites** — toggle which of NOAA 15/18/19 and METEOR-M2 3/4 to capture.
7. **Panel admin** — whether the admin pages need a login, and the password (stored hashed).
8. **Optional sections** — notifications & event feed, daily summary & retention, panel extras
   & HTTPS, advanced SDR/processing. Answer `n` to skip any of them; the defaults are RN2's.
9. **Apply** — the tool uploads everything, runs the installer on the Pi and streams its output.

> **First install on a bare Pi takes a while.** If SatDump is not installed yet it is built from
> source — an hour or more on a Pi 4. The build runs detached on the Pi, so a dropped Wi-Fi
> connection on your PC does not interrupt it; re-running the tool resumes where it left off.

When the installer finishes, rnv3 starts, fetches orbital elements and plans the coming week
of passes. The tool waits for that and then prints the panel address:

```
==> rnv3 is running and has planned its passes.
    Panel: http://raspinoaa/
```

Open it. The **Passes** page shows what will be captured; the first images appear after the
first pass.

### Scripting it

`--save-answers my-station.yaml` writes every answer you gave (passwords excluded);
`--answers my-station.yaml` replays them, so reinstalling a Pi is one command.

---

## 3. Migrate from raspberry-noaa-v2

rnv3 replaces RN2 on the same Pi and **keeps your capture history**. The safe way is in two
runs of the setup tool: first side by side, then cut over.

### Run 1 — side by side (RN2 keeps capturing)

```powershell
.\dist\rnv3-setup.exe
```

- The probe reports your RN2 install (settings, database, capture count).
- Choose **Install rnv3 side by side with raspberry-noaa-v2**.
- Say **yes** to *Prefill answers from raspberry-noaa-v2's settings.yml* — station, SDR, gains,
  enabled satellites, notification tokens and the admin password all carry over; most
  questions become "Enter".
- Say **yes** to *Import raspberry-noaa-v2's history*.

In this mode rnv3 runs with `dry_run: true` (it plans passes but never touches the SDR — RN2
still owns it) and its panel is on **port 8080** because nginx has port 80.

**Validate before cutting over:**

- `http://raspinoaa:8080/passes` against `http://raspinoaa/passes` — the schedules should agree
  to within seconds (SGP4 vs ephem differ that much). Passes rnv3 skips carry the reason in the
  status lamp's tooltip.
- Browse the imported gallery, a capture's enhancements, stats, sky map and admin pages against
  the PHP panel. The import is idempotent; re-run it any time with
  `sudo systemctl stop rnv3 && rnv3-migrate -old ~/raspberry-noaa-v2/db/panel.db -config /etc/rnv3/config.yaml && sudo systemctl start rnv3`.
- Optionally let rnv3 take a real NOAA pass and a real Meteor pass while RN2 is paused: comment
  out the `schedule passes` lines in `crontab -e`, run `atq | awk '{print $1}' | xargs -r atrm`,
  set `scheduling.dry_run: false`, `sudo systemctl restart rnv3`. Check `journalctl -u rnv3`
  (satdump arguments, SNR, frame stats) and the capture page (enhancement set and flips against
  an RN2 capture of the same satellite). **Never let both schedule captures at once**: to hand
  the SDR back to RN2, set `dry_run: true` and restart rnv3 *before* restoring the cron lines.

Live with it for a day or two.

### Run 2 — cut over

```powershell
.\dist\rnv3-setup.exe
```

- Choose **Cut over from raspberry-noaa-v2 to rnv3**.
- The tool first shows a **dry run** of what will change, then asks you to type `CUTOVER`.
- It removes RN2's cron entries and pending `at` jobs, waits for a capture RN2 may be in the
  middle of, stops nginx/php, imports the captures since run 1, moves the panel to port 80 and
  starts rnv3 for real.

RN2's checkout, database and images are left untouched on the Pi. Once rnv3 has taken a few
passes to your satisfaction, delete `~/raspberry-noaa-v2`.

### Doing it by hand over SSH

The setup tool is a front end for two scripts you can also run yourself:

```powershell
.\deploy\deploy.ps1 -PiHost raspinoaa -PiUser pi -InstallArgs "--no-start"   # build, copy, install
./deploy/deploy.sh raspinoaa -user pi -install-args "--no-start"             # macOS / Linux
```

then on the Pi edit `/etc/rnv3/config.yaml` (station and SDR from RN2's `settings.yml`,
`scheduling.dry_run: true`, `web.listen: ":8080"`), `sudo systemctl start rnv3`, validate as
above, and finally:

```bash
cd /tmp/rnv3-deploy
./deploy/cutover.sh --dry-run   # shows what would change
./deploy/cutover.sh             # --kill terminates a running RN2 capture instead of waiting (up to 45 min)
./deploy/cutover.sh --revert    # brings nginx back and stops rnv3; RN2's install_and_upgrade.sh restores its cron/at jobs
```

`install.sh` flags: `--skip-builds` (never compile SatDump/rtl-sdr), `--no-start` (install but
leave the service stopped).

---

## 4. Using the station

| Page | What it shows |
|---|---|
| **Passes** `/passes` | a live terminal (SatDump output streams while a pass is captured; station vitals when idle) and the pass schedule with status lamps |
| **Captures** `/captures` | the gallery, filterable by satellite, day/night and elevation; click a capture for every enhancement plus SNR / frame statistics |
| **Stats** `/stats` | 30-day chart, per-satellite table, daily mosaics and timelapses, the sky-quality map (where your horizon is good and bad) |
| **Admin** `/admin` | unschedule a pass, delete a capture |

There is also a read-only JSON API (`/api/passes`, `/api/captures`, `/api/capture?id=`,
`/api/status`) and an RSS feed (`/api/rss`) — the same endpoints RN2 had.

**Notifications.** Each decoded pass can be pushed to a generic JSON webhook (Home Assistant,
n8n, Node-RED), Discord, Telegram, Pushover and email. The *quality gate* skips weak passes
(low elevation or SNR) for the social channels; the webhook always fires. A daily
*best-of-day* summary with mosaics and timelapses is optional.

**Event feed.** Everything the station does can be pushed to your own website or backend as
webhook events — see [section 7](#7-publishing-to-your-own-website).

**Watchdog.** Once an hour rnv3 checks that captures are still happening, passes are not all
failing, disk is not full, passes are scheduled and (for RTL-SDR) the dongle is still on USB —
and alerts through the same channels, at most once a day per problem.

---

## 5. Changing settings

Run the setup tool again and choose **Reconfigure** — it starts from the current settings and
applies the change (reloading the service, or restarting it for the few settings that need
that). This is the easiest way to enable a notification channel or change satellites later.

Or edit the file directly on the Pi:

```bash
sudo nano /etc/rnv3/config.yaml
sudo systemctl reload rnv3        # applies everything except the four below
```

`web.listen`, `web.tls`, `paths.data_dir` and `scheduling.dry_run` need
`sudo systemctl restart rnv3` instead (the daemon logs a warning and keeps the old value if a
reload changes one of them). Every key is documented in
[config.example.yaml](config.example.yaml).

Handy commands on the Pi:

```bash
journalctl -u rnv3 -f                       # live log
systemctl status rnv3                       # is it running?
rnv3 -config /etc/rnv3/config.yaml -check   # validate a config edit
rnv3 -hash-password                         # make a hash for web.admin.password_hash
rnv3 -config /etc/rnv3/config.yaml -publish-test   # ping every event-feed receiver
```

---

## 6. Upgrading rnv3

```powershell
git pull
.\deploy\release.ps1       # ./deploy/release.sh on macOS / Linux
.\dist\rnv3-setup.exe      # → Reconfigure
```

Reconfigure notices that the tool carries a newer daemon than the Pi runs, ships it, reruns the
installer and restarts the service before applying your (unchanged) settings. The installer is
idempotent: binaries are replaced, your config and data are kept, the SatDump/rtl-sdr builds are
skipped when already present.

Developers with SSH keys on the Pi can skip the tool:
`.\deploy\deploy.ps1 -PiHost raspinoaa -PiUser pi -InstallArgs "--skip-builds"`
(`./deploy/deploy.sh raspinoaa -install-args "--skip-builds"` on macOS / Linux).

---

## 7. Publishing to your own website

rnv3 can send everything that happens at the station to one or more HTTP receivers — every
decoded pass with its image files, failed passes, deletions, the upcoming schedule, a health
sample every five minutes and watchdog alerts. rnv3 knows nothing about the backend: the
receiver stores the events wherever it likes. The full event format is in
[docs/webhooks.md](docs/webhooks.md).

1. Build a receiver that accepts `POST`s with `Authorization: Bearer <secret>` and answers
   `2xx`. ([permi](https://github.com/perhp/permi), a Next.js site on Supabase, has one at
   `app/api/station/webhook/route.ts` you can copy from.)
2. In the setup tool → Reconfigure → *Configure notifications* → *Send station events to a
   webhook receiver*: the receiver's URL and the secret. Or in `config.yaml`:

   ```yaml
   publish:
     backfill_days: 31
     endpoints:
       - name: my-site
         url: https://example.org/api/station/webhook
         token: change-me
         images: true
   ```

3. `rnv3 -config /etc/rnv3/config.yaml -publish-test` sends a test event and prints the
   receiver's answer.

Pass events are queued on the Pi and retried with backoff (up to a week) if the receiver is
down, always in order; on every start rnv3 also re-sends decoded passes newer than
`backfill_days` that a receiver has not acknowledged. Receivers should therefore be
idempotent on the pass id and image name.

---

## 8. Where things live on the Pi

| Path | Content |
|---|---|
| `/etc/rnv3/config.yaml` | the one configuration file (0600, holds your notification tokens) |
| `/var/lib/rnv3/` | database (`rnv3.db`, including the event-feed outbox), cached TLEs, watchdog state |
| `/srv/images/` | captures (`<SAT>-<date>-<time>-<enhancement>.jpg`), polar plots, sky map, mosaics, timelapses |
| `/srv/images/thumb/` | thumbnails |
| `/srv/audio/noaa`, `/srv/audio/meteor` | retained recordings (wav / cadu), pruned after `delete_audio_older_than_days` |
| `/srv/work/` | per-pass SatDump work directories (kept only when a pass fails, swept after 7 days) |
| `/var/ramfs` | tmpfs used to buffer captures in RAM when enough is free |
| `/usr/local/bin/rnv3`, `rnv3-migrate` | the binaries |
| `/usr/share/satdump/satdump_cfg.json` | SatDump's config — **generated by rnv3** from your enhancement settings; don't edit by hand |
| `/tmp/rnv3-deploy/` | where the setup tool / deploy script stage files and scripts (`install.sh`, `cutover.sh`) |

---

## 9. Troubleshooting

**"no successfully decoded capture" alerts / nothing gets captured**
`journalctl -u rnv3 -n 200`. Look for the `satdump starting` line for the pass and what
followed. `no recording produced` usually means the SDR was not found — check
`lsusb` / the *SDR on USB* line in the panel's idle terminal, and that your user is in
`plugdev` (the installer adds it; log out and in again).

**Panel shows no passes**
TLEs could not be fetched (no internet at start?). rnv3 retries; the idle terminal shows the
TLE age. Force a replan with `sudo systemctl reload rnv3`.

**Images come out upside-down / mirrored for Meteor**
`processing.meteor.flip_northbound` — same setting RN2 had.

**Port 80 already in use**
Another web server is running (nginx from RN2?). Either cut over properly (section 3) or set
`web.listen: ":8080"`.

**Locked out of the admin pages**
Set a new hash: `rnv3 -hash-password`, paste it into `web.admin.password_hash`, reload. Or set
`web.admin.enabled: false` for a LAN-only station.

**Events never reach my website**
`rnv3 -config /etc/rnv3/config.yaml -publish-test` says why (`HTTP 401` = secret mismatch,
`404` = wrong URL, `500` = receiver misconfigured). `journalctl -u rnv3 | grep -i publish`
shows queued deliveries failing and their retry interval;
`sqlite3 /var/lib/rnv3/rnv3.db "select event,attempts,count(*) from outbox group by 1,2"`
shows the queue.

**Setup tool: "host key mismatch"**
The Pi was reinstalled and has a new SSH key. Delete its line from
`%APPDATA%\rnv3\known_hosts` and run again.

**Setup tool: the install seems to hang on "Building SatDump"**
It isn't hung, it's compiling; a Pi 4 needs an hour or more. Progress lines keep coming.

---

## 10. Developing rnv3

```
go build ./...
go test ./...
go run ./cmd/rnv3 -config config.example.yaml
go run ./tools/release                        # dist/: Pi binaries + rnv3-setup for this PC
go run ./tools/release -target linux/amd64    # rnv3-setup for another OS/arch
go run ./tools/release -deploy raspinoaa      # build + scp + install.sh on the Pi
```

`-deploy` uses the `ssh`/`scp` commands on your PC (Windows 10+, macOS and Linux all ship
them) and expects your SSH key to be on the Pi; `rnv3-setup` itself needs neither — it carries
its own SSH client and asks for the password.

Everything except SatDump itself runs and is tested on Windows/macOS/Linux; the capture path
is tested against a fake SatDump, the web panel against a seeded database, the setup tool
against an in-process SSH server, the event feed against an in-process receiver.

| Directory | Purpose |
|---|---|
| `cmd/rnv3` | the daemon |
| `cmd/rnv3-setup` | the interactive installer (PC side) |
| `tools/migrate` | RN2 `panel.db` + images importer |
| `internal/config` | YAML schema, defaults, validation, live reload |
| `internal/tle`, `predict`, `sched` | TLE fetch/validation, SGP4, pass planning and the scheduler loop |
| `internal/capture`, `cadu` | SatDump runner, per-SDR flags, SNR/frame statistics |
| `internal/process`, `satdumpcfg` | image rules, thumbnails, polar/sky-map SVG, mosaics/timelapses, SatDump config generator |
| `internal/store` | SQLite (pure Go), migrations, queries, event outbox |
| `internal/web`, `livelog` | panel, JSON API, SSE terminal |
| `internal/notify`, `jobs` | push channels, watchdog, best-of-day, retention |
| `internal/publish`, `hostinfo` | event webhooks and the host readings they carry |
| `internal/setup` | SSH client, probe, wizard, installer orchestration |
| `tools/release` | the build tool: Pi binaries, payload, `rnv3-setup` for any OS, `-deploy` dev fast path |
| `deploy/` | `install.sh`, `cutover.sh`, systemd unit, and the `release`/`deploy` `.ps1`/`.sh` wrappers around `tools/release` |
| `docs/` | the event webhook contract |

[plan.md](plan.md) holds the investigation of RN2, the design decisions and the milestone
history.

---

## 11. Appendix: what the installer sets up

`deploy/install.sh` runs on the Pi (as the station user, sudo where needed) and is idempotent.

| Piece | Detail |
|---|---|
| apt | SatDump/rtl-sdr build dependencies, airspy/hackrf tools |
| osmocom rtl-sdr | built with `DETACH_KERNEL_DRIVER=ON` when `/usr/local/bin/rtl_sdr` is missing (needed for RTL-SDR V4); udev rules |
| kernel blacklists | `dvb_usb_rtl28xxu`/`rtl2832`/`rtl2830`, `airspy`, `msi001`/`msi2500` |
| SatDump | 1.2.2 built from source when `/usr/bin/satdump` is missing (`-j2`, an hour+ on a Pi 4); `/usr/share/satdump` owned by the service user so rnv3 can regenerate `satdump_cfg.json` |
| ramfs | `/var/ramfs` tmpfs 1000M in fstab |
| binaries | `/usr/local/bin/rnv3`, `/usr/local/bin/rnv3-migrate` |
| config | `/etc/rnv3/config.yaml`, 0600, owned by the service user (created from the example when absent, never overwritten) |
| dirs | `/var/lib/rnv3`, `/srv/images{,/thumb}`, `/srv/audio/{noaa,meteor}`, `/srv/work` |
| service | `rnv3.service`: station user + `plugdev`, `CAP_NET_BIND_SERVICE` for port 80, config check before start, 95 s stop timeout so an in-flight capture can finish |
