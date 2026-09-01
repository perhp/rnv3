#!/usr/bin/env bash
#
# rnv3 on-device installer (Debian Trixie on a Raspberry Pi or x64).
# Idempotent: re-run after every deploy. Run as the station user (not root);
# it sudos where needed.
#
#   ./deploy/install.sh [--skip-builds] [--no-start] [path-to-rnv3-binary]
#
#   --skip-builds  never build SatDump / osmocom rtl-sdr from source, even
#                  when they are missing (you install them yourself)
#   --no-start     install everything but leave the service stopped
#                  (side-by-side phase next to a running RN2)
#
# What it sets up, replacing RN2's Ansible roles:
#   - apt runtime/build dependencies
#   - SatDump 1.2.2 built from source when /usr/bin/satdump is missing
#   - osmocom rtl-sdr (DETACH_KERNEL_DRIVER, needed for RTL-SDR V4) when
#     /usr/local/bin/rtl_sdr is missing; udev rules; DVB/Airspy/MiriSDR
#     kernel module blacklists; plugdev membership
#   - /var/ramfs tmpfs (fstab) for in-memory capture buffering
#   - /usr/local/bin/rnv3 + rnv3-migrate, /etc/rnv3/config.yaml (0600),
#     data/image/audio/work directories, systemd unit
set -euo pipefail

SATDUMP_VERSION="1.2.2"
RAMFS_PATH="/var/ramfs"
RAMFS_SIZE="1000M"

if [ "$EUID" -eq 0 ]; then
  echo "Run as the station user, not root." >&2
  exit 1
fi

SKIP_BUILDS=0
NO_START=0
BINARY=""
for arg in "$@"; do
  case "$arg" in
    --skip-builds) SKIP_BUILDS=1 ;;
    --no-start) NO_START=1 ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) BINARY="$arg" ;;
  esac
done
HERE="$(cd "$(dirname "$0")" && pwd)"
BINARY="${BINARY:-$HERE/../rnv3}"
[ -f "$BINARY" ] || BINARY="$HERE/rnv3"
if [ ! -f "$BINARY" ]; then
  echo "rnv3 binary not found (looked for $BINARY). Usage: $0 [--skip-builds] [--no-start] [path-to-rnv3]" >&2
  exit 1
fi
MIGRATE="$(dirname "$BINARY")/rnv3-migrate"

step() { echo; echo "==> $*"; }

# ---------------------------------------------------------------------------
step "apt dependencies"
# Runtime: what SatDump links against, plus the SDR userspace tools. Build
# deps only matter when a source build runs below, but they are cheap and
# keep this list in one place (RN2 dependencies.yml + sdr.yml).
sudo apt-get update -q
sudo DEBIAN_FRONTEND=noninteractive apt-get install -yq --no-install-recommends \
  build-essential cmake git curl pkg-config pkgconf g++ \
  libusb-1.0-0 libusb-1.0-0-dev \
  airspy hackrf \
  libfftw3-dev libpng-dev libtiff-dev libjemalloc-dev libcurl4-openssl-dev \
  libvolk-dev libnng-dev libzstd-dev libomp-dev libglfw3-dev libgles-dev libegl-dev \
  portaudio19-dev libhdf5-dev librtlsdr-dev libhackrf-dev libairspy-dev libairspyhf-dev \
  ocl-icd-opencl-dev

# ---------------------------------------------------------------------------
step "osmocom rtl-sdr"
if [ -x /usr/local/bin/rtl_sdr ]; then
  echo "    present: /usr/local/bin/rtl_sdr"
elif [ "$SKIP_BUILDS" = 1 ]; then
  echo "    missing, and --skip-builds given: RTL-SDR V4 support depends on the osmocom build" >&2
else
  # The osmocom build with kernel-driver detach is what gives SatDump working
  # RTL-SDR V4 support; Trixie's stock librtlsdr stays as a build dependency
  # and the /usr/local library wins at runtime via ldconfig.
  rm -rf /tmp/rtl-sdr
  git clone -q --depth 1 https://github.com/osmocom/rtl-sdr.git /tmp/rtl-sdr
  mkdir -p /tmp/rtl-sdr/build
  (cd /tmp/rtl-sdr/build && cmake .. -DDETACH_KERNEL_DRIVER=ON > /dev/null && make -j2)
  (cd /tmp/rtl-sdr/build && sudo make install > /dev/null)
  sudo ldconfig
  sudo install -m 0644 /tmp/rtl-sdr/rtl-sdr.rules /etc/udev/rules.d/rtl-sdr.rules
  echo "    built and installed"
fi
if [ ! -f /etc/udev/rules.d/rtl-sdr.rules ]; then
  if [ -f /tmp/rtl-sdr/rtl-sdr.rules ]; then
    sudo install -m 0644 /tmp/rtl-sdr/rtl-sdr.rules /etc/udev/rules.d/rtl-sdr.rules
  else
    curl -fsSL https://raw.githubusercontent.com/osmocom/rtl-sdr/master/rtl-sdr.rules | sudo tee /etc/udev/rules.d/rtl-sdr.rules > /dev/null
  fi
  echo "    installed /etc/udev/rules.d/rtl-sdr.rules"
fi

# ---------------------------------------------------------------------------
step "SDR kernel module blacklists and udev rules"
write_root_file() { # path content...
  local path="$1"; shift
  if [ ! -f "$path" ] || [ "$(cat "$path")" != "$*" ]; then
    printf '%s\n' "$@" | sudo tee "$path" > /dev/null
    echo "    wrote $path"
  fi
}
write_root_file /etc/modprobe.d/rtlsdr.conf "blacklist dvb_usb_rtl28xxu" "blacklist rtl2832" "blacklist rtl2830"
write_root_file /etc/modprobe.d/airspy-blacklist.conf "blacklist airspy"
write_root_file /etc/modprobe.d/blacklist-msi.conf "blacklist msi001" "blacklist msi2500"
write_root_file /etc/udev/rules.d/52-airspy.rules \
  'ATTR{idVendor}=="1d50", ATTR{idProduct}=="60a1", SYMLINK+="airspy-%k", MODE="660", GROUP="plugdev"'
sudo modprobe -r dvb_usb_rtl28xxu rtl2832 rtl2830 msi2500 msi001 airspy 2> /dev/null || true
sudo udevadm control --reload-rules && sudo udevadm trigger || true
if ! id -nG "$USER" | grep -qw plugdev; then
  sudo usermod -aG plugdev "$USER"
  echo "    added $USER to plugdev (takes effect for the service immediately; re-login for your shell)"
fi

# ---------------------------------------------------------------------------
step "SatDump"
if [ -x /usr/bin/satdump ]; then
  echo "    present: $(/usr/bin/satdump --version 2>/dev/null | head -1 || echo /usr/bin/satdump)"
elif [ "$SKIP_BUILDS" = 1 ]; then
  echo "    missing, and --skip-builds given: rnv3 cannot capture without it" >&2
else
  # No Trixie package upstream: build against the distro's volk/nng. -j2
  # rather than nproc: the C++ units are memory-hungry and four jobs can
  # OOM a 4 GB Pi. Expect an hour or more on a Pi 4.
  rm -rf /tmp/satdump
  git clone -q --depth 1 --branch "$SATDUMP_VERSION" https://github.com/SatDump/SatDump.git /tmp/satdump
  mkdir -p /tmp/satdump/build
  (cd /tmp/satdump/build && cmake -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr .. > /dev/null && make -j2)
  (cd /tmp/satdump/build && sudo make install > /dev/null)
  echo "    built and installed SatDump $SATDUMP_VERSION"
fi
# rnv3 regenerates satdump_cfg.json from config.yaml with an atomic write, so
# the service user must own the DIRECTORY, not just the file.
if [ -d /usr/share/satdump ]; then
  sudo chown "$USER:$USER" /usr/share/satdump
  [ -f /usr/share/satdump/satdump_cfg.json ] && sudo chown "$USER:$USER" /usr/share/satdump/satdump_cfg.json
fi
# SatDump writes ~/.config/satdump on first run; on Lite images the top
# directory otherwise ends up root-owned.
mkdir -p "$HOME/.config" && chmod 0700 "$HOME/.config"

# ---------------------------------------------------------------------------
step "ramfs ($RAMFS_PATH, tmpfs $RAMFS_SIZE)"
sudo mkdir -p "$RAMFS_PATH"
if ! grep -qE "^tmpfs[[:space:]]+$RAMFS_PATH[[:space:]]" /etc/fstab; then
  echo "tmpfs $RAMFS_PATH tmpfs nodev,nosuid,size=$RAMFS_SIZE 0 0" | sudo tee -a /etc/fstab > /dev/null
  echo "    added fstab entry"
fi
if ! mountpoint -q "$RAMFS_PATH"; then
  sudo mount "$RAMFS_PATH"
  echo "    mounted"
fi
sudo chown "$USER:$USER" "$RAMFS_PATH" && sudo chmod 0770 "$RAMFS_PATH"

# ---------------------------------------------------------------------------
step "binaries"
sudo install -m 0755 "$BINARY" /usr/local/bin/rnv3
if [ -f "$MIGRATE" ]; then
  sudo install -m 0755 "$MIGRATE" /usr/local/bin/rnv3-migrate
fi
echo "    $(/usr/local/bin/rnv3 -version)"

# ---------------------------------------------------------------------------
step "config"
sudo mkdir -p /etc/rnv3
if [ ! -f /etc/rnv3/config.yaml ]; then
  # 0600, owned by the service user: it holds notification credentials.
  sudo install -m 0600 -o "$USER" -g "$USER" "$HERE/../config.example.yaml" /etc/rnv3/config.yaml
  echo "    wrote /etc/rnv3/config.yaml from the example — edit station/satellites/sdr, then re-run"
else
  sudo chown "$USER:$USER" /etc/rnv3/config.yaml
  sudo chmod 0600 /etc/rnv3/config.yaml
fi

# ---------------------------------------------------------------------------
step "directories"
sudo mkdir -p /var/lib/rnv3 /srv/images/thumb /srv/audio/noaa /srv/audio/meteor /srv/work
sudo chown -R "$USER:$USER" /var/lib/rnv3 /srv/images /srv/audio /srv/work

# ---------------------------------------------------------------------------
step "validating config"
/usr/local/bin/rnv3 -config /etc/rnv3/config.yaml -check

# ---------------------------------------------------------------------------
step "systemd unit"
sed "s/__USER__/$USER/g" "$HERE/rnv3.service" | sudo tee /etc/systemd/system/rnv3.service > /dev/null
sudo systemctl daemon-reload
if [ "$NO_START" = 1 ]; then
  sudo systemctl enable rnv3 > /dev/null
  if systemctl is-active --quiet rnv3; then
    sudo systemctl stop rnv3
    echo "    stopped the running service (--no-start)"
  fi
  echo "    enabled, not running (--no-start). Start with: sudo systemctl start rnv3"
else
  if systemctl is-active --quiet nginx && grep -qE 'listen:[[:space:]]*":?80"' /etc/rnv3/config.yaml; then
    echo "    WARNING: nginx is active and web.listen is :80 — rnv3 will fail to bind." >&2
    echo "             For a side-by-side run set web.listen to \":8080\"; deploy/cutover.sh retires nginx." >&2
  fi
  sudo systemctl enable rnv3 > /dev/null
  sudo systemctl restart rnv3
  sleep 1
  systemctl --no-pager --lines=5 status rnv3 || true
fi

echo
echo "==> Done. Log: journalctl -u rnv3 -f"
