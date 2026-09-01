#!/usr/bin/env bash
# Thin wrapper for the release tool (macOS / Linux): finds Go (even when it is
# not on PATH) and runs `go run ./tools/release` with whatever arguments you pass.
#
#   ./deploy/release.sh                        # dist/rnv3-setup for this PC
#   ./deploy/release.sh -target windows/amd64  # the setup tool for a Windows PC
#   ./deploy/release.sh -arch amd64            # Pi binaries for an x64 station
#
# All build logic lives in tools/release/main.go.
set -euo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"

go_bin="$(command -v go 2>/dev/null || true)"
if [ -z "$go_bin" ]; then
    for candidate in /usr/local/go/bin/go /opt/homebrew/bin/go /usr/lib/go/bin/go \
                     "$HOME/go/bin/go" "$HOME/sdk/go/bin/go" "$HOME/.local/go/bin/go" \
                     "${LOCALAPPDATA:-}/go/bin/go.exe" "${PROGRAMFILES:-}/Go/bin/go.exe"; do  # Git Bash on Windows
        if [ -x "$candidate" ]; then go_bin="$candidate"; break; fi
    done
fi
if [ -z "$go_bin" ]; then
    echo "Go was not found. Install it from https://go.dev/dl/ (or put go on your PATH)." >&2
    exit 1
fi

exec "$go_bin" run -C "$repo" ./tools/release "$@"
