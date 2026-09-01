# rnv3

**A NOAA APT / Meteor-M LRPT weather-satellite ground station in one binary.**

rnv3 is the rewrite of [raspberry-noaa-v2](https://github.com/perhp/raspberry-noaa-v2) (RN2).
Where RN2 was bash + Python + PHP + Ansible + nginx + cron + `at`, rnv3 is a single Go daemon
that does everything itself:

- predicts passes (SGP4), schedules and captures them with [SatDump](https://github.com/SatDump/SatDump)
- produces the enhancements, thumbnails, polar plots, sky-quality map, daily mosaics and timelapses
- serves the web panel (live terminal, schedule, gallery, stats, admin) on port 80
- pushes captures to a webhook, Discord, Telegram, Pushover or email
- watches its own health, prunes old captures, keeps its TLEs fresh

It runs on 64-bit Raspberry Pi OS / Debian 13 (Trixie) on a Pi 3/4/5, and you set it up from
your PC without ever opening a terminal on the Pi.

---

## Contents

1. [What you need](#1-what-you-need)
2. [Install on a new Pi](#2-install-on-a-new-pi)
3. [Migrate from raspberry-noaa-v2](#3-migrate-from-raspberry-noaa-v2)
4. [Using the station](#4-using-the-station)
5. [Changing settings](#5-changing-settings)
6. [Where things live on the Pi](#6-where-things-live-on-the-pi)
7. [Troubleshooting](#7-troubleshooting)
8. [Developing rnv3](#8-developing-rnv3)

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

**On your PC** (Windows; macOS/Linux work too with the same commands)

- [Go](https://go.dev/dl/) 1.27 or newer, to build the tools once (an older 1.21+ Go fetches it
  automatically; the scripts find Go in its usual install folder even if it is not on your PATH)
- Git, to clone this repository

That's all. SatDump and the SDR drivers are installed on the Pi by rnv3 itself.

---

## 2. Install on a new Pi

### Step 1 — build the setup tool

```powershell
git clone https://github.com/perhp/rnv3.git
cd rnv3
.\deploy\release.ps1
```

This cross-compiles the Pi binaries and packs them into **`dist\rnv3-setup.exe`** — a
self-contained installer for your PC. You only rebuild it when you update rnv3.

### Step 2 — run it

```powershell
.\dist\rnv3-setup.exe
```

The tool asks a series of questions. Press **Enter** to accept the value shown in `[brackets]`.

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
8. **Optional sections** — notifications, daily summary & retention, panel extras & HTTPS,
   advanced SDR/processing. Answer `n` to skip any of them; the defaults are RN2's.
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

Now compare: `http://raspinoaa:8080/passes` against `http://raspinoaa/passes`. The schedules
should agree to within seconds. Browse the imported gallery, stats and sky map. Live with it
for a day or two.

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

(If you prefer doing this by hand over SSH, [deploy/README.md](deploy/README.md) walks
through the same steps with `install.sh` and `cutover.sh`.)

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
`sudo systemctl restart rnv3` instead. Every key is documented in
[config.example.yaml](config.example.yaml).

Handy commands on the Pi:

```bash
journalctl -u rnv3 -f                   # live log
systemctl status rnv3                   # is it running?
rnv3 -config /etc/rnv3/config.yaml -check   # validate a config edit
rnv3 -hash-password                     # make a hash for web.admin.password_hash
```

---

## 6. Where things live on the Pi

| Path | Content |
|---|---|
| `/etc/rnv3/config.yaml` | the one configuration file (0600, holds your notification tokens) |
| `/var/lib/rnv3/` | database (`rnv3.db`), cached TLEs, watchdog state |
| `/srv/images/` | captures (`<SAT>-<date>-<time>-<enhancement>.jpg`), polar plots, sky map, mosaics, timelapses |
| `/srv/images/thumb/` | thumbnails |
| `/srv/audio/noaa`, `/srv/audio/meteor` | retained recordings (wav / cadu), pruned after `delete_audio_older_than_days` |
| `/srv/work/` | per-pass SatDump work directories (kept only when a pass fails, swept after 7 days) |
| `/var/ramfs` | tmpfs used to buffer captures in RAM when enough is free |
| `/usr/local/bin/rnv3`, `rnv3-migrate` | the binaries |
| `/usr/share/satdump/satdump_cfg.json` | SatDump's config — **generated by rnv3** from your enhancement settings; don't edit by hand |

---

## 7. Troubleshooting

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

**Setup tool: "host key mismatch"**
The Pi was reinstalled and has a new SSH key. Delete its line from
`%APPDATA%\rnv3\known_hosts` and run again.

**Setup tool: the install seems to hang on "Building SatDump"**
It isn't hung, it's compiling; a Pi 4 needs an hour or more. Progress lines keep coming.

---

## 8. Developing rnv3

```
go build ./...
go test ./...
go run ./cmd/rnv3 -config config.example.yaml
```

Everything except SatDump itself runs and is tested on Windows/macOS/Linux; the capture path
is tested against a fake SatDump, the web panel against a seeded database, the setup tool
against an in-process SSH server.

| Directory | Purpose |
|---|---|
| `cmd/rnv3` | the daemon |
| `cmd/rnv3-setup` | the interactive installer (PC side) |
| `tools/migrate` | RN2 `panel.db` + images importer |
| `internal/config` | YAML schema, defaults, validation, live reload |
| `internal/tle`, `predict`, `sched` | TLE fetch/validation, SGP4, pass planning and the scheduler loop |
| `internal/capture`, `cadu` | SatDump runner, per-SDR flags, SNR/frame statistics |
| `internal/process`, `satdumpcfg` | image rules, thumbnails, polar/sky-map SVG, mosaics/timelapses, SatDump config generator |
| `internal/store` | SQLite (pure Go), migrations, queries |
| `internal/web`, `livelog` | panel, JSON API, SSE terminal |
| `internal/notify`, `jobs` | push channels, watchdog, best-of-day, retention |
| `internal/setup` | SSH client, probe, wizard, installer orchestration |
| `deploy/` | `install.sh`, `cutover.sh`, systemd unit, `release.ps1`, `deploy.ps1` (dev fast path) |

`.\deploy\deploy.ps1 -PiHost raspinoaa -PiUser pi` builds and installs straight onto a Pi you
already have SSH access to — the developer's shortcut around the setup tool.

[plan.md](plan.md) holds the investigation of RN2, the design decisions and the milestone
history.
