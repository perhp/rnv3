#!/usr/bin/env bash
# Dev fast path (macOS / Linux): build the Pi binaries, copy them over ssh and
# run install.sh. Thin wrapper around `go run ./tools/release -deploy`.
#
#   ./deploy/deploy.sh [host] [extra release flags...]
#   ./deploy/deploy.sh raspinoaa -user pi -install-args "--skip-builds --no-start"
#   ./deploy/deploy.sh raspinoaa -no-install
set -euo pipefail
host="${1:-raspinoaa}"
[ $# -gt 0 ] && shift
exec "$(dirname "$0")/release.sh" -deploy "$host" "$@"
