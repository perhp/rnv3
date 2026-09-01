# rnv3

Single-binary rewrite of [raspberry-noaa-v2](https://github.com/perhp/raspberry-noaa-v2):
a NOAA APT / Meteor-M LRPT ground station daemon for Debian Trixie (Raspberry Pi 3/4/5 or x64).

One Go process replaces bash + Python + PHP + Ansible + nginx + cron + `at`. The only external
runtime dependencies are [SatDump](https://github.com/SatDump/SatDump) and the SDR driver stack.

**Status: M0 (skeleton).** See [plan.md](plan.md) for the full investigation of the old system,
the architecture, and the milestone roadmap.

## Develop (Windows or anywhere)

```
go build ./...
go test ./...
go run ./cmd/rnv3 -config config.example.yaml
```

## Set up a Pi (no terminal on the Pi needed)

```
.\deploy\release.ps1      # builds dist\rnv3-setup.exe with the Pi binaries embedded
.\dist\rnv3-setup.exe
```

`rnv3-setup` connects to the Pi over SSH (password login), looks around (OS, SatDump, SDR on
USB, an existing raspberry-noaa-v2), asks the questions — prefilled from RN2's `settings.yml`
when it finds one — writes `/etc/rnv3/config.yaml`, ships the binaries, runs the installer
(building SatDump/rtl-sdr from source when missing), optionally imports RN2's capture history,
and waits for the first pass plan. Run it again to reconfigure, or to cut over from RN2 when
you've validated rnv3 side by side. `--answers file.yaml` scripts a run; `--save-answers`
writes one (passwords excluded).

## Developer deploy

```
.\deploy\deploy.ps1 -PiHost raspinoaa -PiUser <user>
```

Cross-compiles static linux/arm64 binaries (`rnv3`, `rnv3-migrate`), copies them over SSH, and
runs `deploy/install.sh` (SatDump/rtl-sdr builds when missing, udev/blacklists, ramfs, binaries →
`/usr/local/bin`, config → `/etc/rnv3/config.yaml`, systemd unit `rnv3.service`).

Migrating from raspberry-noaa-v2 — side-by-side validation, then `deploy/cutover.sh` — is
walked through in [deploy/README.md](deploy/README.md).

## Layout

```
cmd/rnv3/            entrypoint
internal/config/     YAML schema, defaults, validation
internal/store/      SQLite (pure Go), versioned migrations
internal/web/        HTTP server + embedded UI
deploy/              systemd unit, installer, deploy script
```
