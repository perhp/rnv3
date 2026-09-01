# Deploying rnv3 and cutting over from raspberry-noaa-v2

Everything runs from the Windows dev box with `deploy/deploy.ps1`; the Pi only
ever receives two static binaries, three scripts and the example config.

## 1. First deploy — side by side with RN2

RN2 keeps running (cron/at capture the passes, nginx serves the panel on :80).
rnv3 runs next to it in dry-run mode, planning but never touching the SDR.

```powershell
.\deploy\deploy.ps1 -InstallArgs "--no-start"
```

Then on the Pi, edit `/etc/rnv3/config.yaml`:

- `station:` latitude/longitude/altitude/location and `sdr.type`, from RN2's
  `config/settings.yml`
- `satellites:` enable the same birds as RN2 (the example enables only the
  two Meteors); gains and elevations from `settings.yml`
- `scheduling.dry_run: true`
- `web.listen: ":8080"` (nginx has :80)
- notifications: leave disabled for now

```bash
sudo systemctl start rnv3
journalctl -u rnv3 -f
```

**Validate (the M1–M4 exit criteria):**

- `http://raspinoaa:8080/passes` — the schedule should match RN2's
  `http://raspinoaa/passes` to within seconds (SGP4 vs ephem). Passes rnv3
  skips carry the reason in the status lamp tooltip.
- Import history so the gallery/stats can be compared against real data:
  `sudo systemctl stop rnv3 && rnv3-migrate -old ~/raspberry-noaa-v2/db/panel.db -config /etc/rnv3/config.yaml && sudo systemctl start rnv3`
  (idempotent — re-run any time). Walk through captures, enhancements, stats,
  sky map, admin pages against the PHP panel.
- `go test -race` on the Pi if you have Go there (the Windows box has no C
  toolchain): `GOFLAGS=-race go test ./...` in a checkout.

## 2. Real captures with rnv3

Two processes must not share the SDR. Pause RN2's scheduling for the test
window (`crontab -e`, comment out the `schedule passes` lines, then
`atq | awk '{print $1}' | xargs -r atrm`), set `scheduling.dry_run: false`,
`sudo systemctl restart rnv3`, and let it take a NOAA pass and a Meteor pass.

Check per pass: `journalctl -u rnv3` (satdump args, SNR, frame stats), the
capture page (enhancement set and flips vs an RN2 capture of the same
satellite), `/srv/images` naming. Enable notification channels one at a time
and watch the log for the push results.

If you want more side-by-side time afterwards, take rnv3 off the SDR **before**
restoring RN2's cron lines: set `scheduling.dry_run: true` and
`sudo systemctl restart rnv3` (or `sudo systemctl stop rnv3`), then put the
cron lines back. Never let both schedule captures at once.

## 3. Cutover

```bash
./deploy/cutover.sh --dry-run   # shows what would change
./deploy/cutover.sh
```

It removes RN2's cron entries and pending `at` jobs, waits for a capture RN2
may already be running (`--kill` terminates it instead), disables
nginx/php-fpm, imports `panel.db` once more (catching captures since the last
import), moves `web.listen` back to `:80`, and starts rnv3. RN2's checkout, database and
images are left in place; `./deploy/cutover.sh --revert` brings nginx back
and stops rnv3 (RN2's `install_and_upgrade.sh` restores its cron/at jobs).

Run it as the sole owner of the SDR for a week, then delete
`~/raspberry-noaa-v2`.

## Later deploys

```powershell
.\deploy\deploy.ps1 -InstallArgs "--skip-builds"
```

`install.sh` is idempotent: it re-installs the binaries, keeps your config,
and restarts the service. Config-only changes don't need a deploy:
`sudo systemctl reload rnv3` (SIGHUP) applies everything except
`web.listen`, `web.tls`, `paths.data_dir` and `scheduling.dry_run`, which
need `sudo systemctl restart rnv3` (the daemon logs a warning and keeps the
old value if a reload changes one of them).

## What install.sh sets up

| Piece | Detail |
|---|---|
| apt | SatDump/rtl-sdr build deps, airspy/hackrf tools |
| osmocom rtl-sdr | built with `DETACH_KERNEL_DRIVER=ON` when `/usr/local/bin/rtl_sdr` is missing (RTL-SDR V4); udev rules |
| kernel blacklists | `dvb_usb_rtl28xxu`/`rtl2832`/`rtl2830`, `airspy`, `msi001`/`msi2500` |
| SatDump | 1.2.2 built from source when `/usr/bin/satdump` is missing (`-j2`, an hour+ on a Pi 4); `/usr/share/satdump` owned by the service user so rnv3 can regenerate `satdump_cfg.json` |
| ramfs | `/var/ramfs` tmpfs 1000M in fstab |
| binaries | `/usr/local/bin/rnv3`, `/usr/local/bin/rnv3-migrate` |
| config | `/etc/rnv3/config.yaml`, 0600, owned by the service user |
| dirs | `/var/lib/rnv3`, `/srv/images{,/thumb}`, `/srv/audio/{noaa,meteor}`, `/srv/work` |
| service | `rnv3.service`: station user + plugdev, `CAP_NET_BIND_SERVICE`, config check before start, 95 s stop timeout |
