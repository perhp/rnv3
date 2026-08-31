#!/usr/bin/env bash
#
# rnv3 on-device installer (Debian Trixie, Pi or x64). Idempotent.
# Run as the station user (not root); it sudos where needed.
#
# M0 scope: binary + config + dirs + systemd unit. Later milestones extend
# this with SatDump/osmocom rtl-sdr source builds, udev rules, and the ramfs
# fstab entry (see plan.md).
set -euo pipefail

if [ "$EUID" -eq 0 ]; then
  echo "Run as the station user, not root." >&2
  exit 1
fi

BINARY="${1:-./rnv3}"
if [ ! -f "$BINARY" ]; then
  echo "Usage: $0 [path-to-rnv3-binary]" >&2
  exit 1
fi

echo "==> Installing binary"
sudo install -m 0755 "$BINARY" /usr/local/bin/rnv3

echo "==> Config"
sudo mkdir -p /etc/rnv3
if [ ! -f /etc/rnv3/config.yaml ]; then
  sudo install -m 0644 "$(dirname "$0")/../config.example.yaml" /etc/rnv3/config.yaml
  echo "    Wrote /etc/rnv3/config.yaml from example — edit it, then re-run or restart."
fi

echo "==> Directories"
sudo mkdir -p /var/lib/rnv3 /srv/images/thumb /srv/audio/noaa /srv/audio/meteor /srv/work
sudo chown -R "$USER:$USER" /var/lib/rnv3 /srv/images /srv/audio /srv/work

echo "==> Validating config"
/usr/local/bin/rnv3 -config /etc/rnv3/config.yaml -check

echo "==> systemd unit"
sed "s/__USER__/$USER/g" "$(dirname "$0")/rnv3.service" | sudo tee /etc/systemd/system/rnv3.service > /dev/null
sudo systemctl daemon-reload
sudo systemctl enable --now rnv3

echo "==> Done"
systemctl --no-pager status rnv3 || true
