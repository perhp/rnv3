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

## Deploy to the Pi

```
.\deploy\deploy.ps1 -PiHost raspinoaa -PiUser <user>
```

Cross-compiles a static linux/arm64 binary, copies it over SSH, and runs `deploy/install.sh`
(binary → `/usr/local/bin/rnv3`, config → `/etc/rnv3/config.yaml`, systemd unit `rnv3.service`).

## Layout

```
cmd/rnv3/            entrypoint
internal/config/     YAML schema, defaults, validation
internal/store/      SQLite (pure Go), versioned migrations
internal/web/        HTTP server + embedded UI
deploy/              systemd unit, installer, deploy script
```
