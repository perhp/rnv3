#!/usr/bin/env bash
#
# Retire raspberry-noaa-v2 on this station and hand the SDR to rnv3.
# Run as the station user after deploy/install.sh has installed rnv3 and
# you have validated it side by side (see deploy/README.md).
#
#   ./deploy/cutover.sh [--dry-run] [--kill] [--rn2-home ~/raspberry-noaa-v2]
#   ./deploy/cutover.sh --revert
#
# Forward:
#   1. remove RN2's cron entries (scheduling, watchdog, sky map, best of day,
#      /run/user perms fix) and every pending 'at' capture job
#   2. wait for a capture RN2 already started to finish (atrm cannot stop a
#      running receive script; up to 45 min, or --kill terminates it) so the
#      two stacks never hold the SDR or write panel.db at the same time
#   3. stop + disable nginx and php-fpm (frees :80 for rnv3)
#   4. import RN2's panel.db + /srv/images into rnv3 (rnv3-migrate; safe to
#      re-run) — with rnv3 stopped so the two never write the db together
#   5. set web.listen back to ":80" if it was moved for the side-by-side run
#   6. start rnv3
# The RN2 checkout, its database and the images are left untouched.
#
# --revert re-enables nginx/php-fpm and stops rnv3; RN2's cron/at jobs come
# back by running RN2's own install_and_upgrade.sh.
set -euo pipefail

DRY_RUN=0
REVERT=0
KILL=0
RN2_HOME="$HOME/raspberry-noaa-v2"
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --revert) REVERT=1 ;;
    --kill) KILL=1 ;;
    --rn2-home) shift; RN2_HOME="$1" ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

run() { # echo, then execute unless dry-run
  echo "    \$ $*"
  [ "$DRY_RUN" = 1 ] || eval "$@"
}
step() { echo; echo "==> $*"; }

if [ "$REVERT" = 1 ]; then
  step "stopping rnv3"
  run sudo systemctl disable --now rnv3
  step "re-enabling the RN2 web stack"
  run sudo systemctl enable --now php8.4-fpm nginx
  echo
  echo "rnv3 stopped and nginx is back. To restore RN2's cron/at scheduling run:"
  echo "    cd $RN2_HOME && ./install_and_upgrade.sh"
  exit 0
fi

step "preflight"
[ -x /usr/local/bin/rnv3 ] || { echo "rnv3 is not installed — run deploy/install.sh first" >&2; exit 1; }
[ -f /etc/rnv3/config.yaml ] || { echo "/etc/rnv3/config.yaml missing" >&2; exit 1; }
/usr/local/bin/rnv3 -config /etc/rnv3/config.yaml -check
if grep -qE '^\s*dry_run:\s*true' /etc/rnv3/config.yaml; then
  echo "    scheduling.dry_run is true in /etc/rnv3/config.yaml — set it to false before cutting over" >&2
  exit 1
fi
OLD_DB="$RN2_HOME/db/panel.db"
[ -f "$OLD_DB" ] && echo "    RN2 database: $OLD_DB" || echo "    no RN2 database at $OLD_DB (skipping import)"
[ "$DRY_RUN" = 1 ] && echo "    DRY RUN: nothing below is executed"

# ---------------------------------------------------------------------------
step "RN2 cron entries"
# Ansible writes each job as a '#Ansible: <name>' marker followed by the job
# line; drop the marker and the line for every RN2 job name.
RN2_JOBS='^#Ansible: (schedule passes|schedule passes daily|schedule passes after reboot|station health watchdog|regenerate reception quality sky map|daily best capture summary push|Correct /run/user/uid perms after reboot)$'
current="$(crontab -l 2>/dev/null || true)"
if [ -n "$current" ] && grep -qE "$RN2_JOBS" <<< "$current"; then
  grep -E "$RN2_JOBS" <<< "$current" | sed 's/^/    removing: /'
  filtered="$(awk -v pat="$RN2_JOBS" '$0 ~ pat { skip = 1; next } skip { skip = 0; next } { print }' <<< "$current")"
  if [ "$DRY_RUN" = 0 ]; then
    if [ -z "$(tr -d '[:space:]' <<< "$filtered")" ]; then
      crontab -r
    else
      printf '%s\n' "$filtered" | crontab -
    fi
  fi
else
  echo "    none present"
fi

step "pending 'at' capture jobs"
pending="$(atq 2>/dev/null | awk '{print $1}' || true)"
if [ -n "$pending" ]; then
  echo "    removing $(wc -w <<< "$pending") job(s)"
  [ "$DRY_RUN" = 0 ] && echo "$pending" | xargs -r atrm
else
  echo "    none pending"
fi

# ---------------------------------------------------------------------------
step "captures RN2 already started"
# atrm only drops queued jobs; a receive_noaa.sh / receive_meteor.sh that is
# mid-pass keeps the SDR and still writes panel.db when it finishes.
rn2_capture_pids() { pgrep -f 'scripts/receive_(noaa|meteor)\.sh' || true; }
if [ -n "$(rn2_capture_pids)" ]; then
  if [ "$KILL" = 1 ]; then
    echo "    terminating running RN2 capture(s) (--kill)"
    if [ "$DRY_RUN" = 0 ]; then
      rn2_capture_pids | xargs -r kill
      pkill -x satdump || true
      sleep 5
      rn2_capture_pids | xargs -r kill -9
      pkill -9 -x satdump || true
    fi
  elif [ "$DRY_RUN" = 1 ]; then
    echo "    a capture is running now; the real run waits for it (or use --kill)"
  else
    # A pass is ~15 min plus processing; give it 45 before giving up.
    waited=0
    while [ -n "$(rn2_capture_pids)" ]; do
      if [ "$waited" -ge 2700 ]; then
        echo "    still running after 45 min — aborting. Re-run, or use --kill." >&2
        exit 1
      fi
      [ $((waited % 60)) -eq 0 ] && echo "    waiting for RN2's capture to finish (${waited}s)..."
      sleep 15
      waited=$((waited + 15))
    done
    echo "    finished"
  fi
else
  echo "    none running"
fi

# ---------------------------------------------------------------------------
step "RN2 web stack (nginx, php-fpm)"
for svc in nginx php8.4-fpm; do
  if systemctl list-unit-files "$svc.service" --no-legend 2>/dev/null | grep -q .; then
    run sudo systemctl disable --now "$svc"
  fi
done

# ---------------------------------------------------------------------------
step "database import"
run sudo systemctl stop rnv3
if [ -f "$OLD_DB" ] && [ -x /usr/local/bin/rnv3-migrate ]; then
  run /usr/local/bin/rnv3-migrate -old "$OLD_DB" -config /etc/rnv3/config.yaml
elif [ -f "$OLD_DB" ]; then
  echo "    rnv3-migrate not installed — deploy with the migrate binary to import history" >&2
fi

# ---------------------------------------------------------------------------
step "web.listen"
# The value may be bare, single- or double-quoted depending on who wrote the
# file (rnv3-setup's YAML encoder leaves it bare). Only the first listen: is
# the HTTP one; the TLS block's comes later and is left alone.
if grep -qE "^[[:space:]]*listen:[[:space:]]*[\"']?:8080[\"']?[[:space:]]*$" /etc/rnv3/config.yaml; then
  echo "    moving web.listen from :8080 (side-by-side) back to :80"
  run "sed -i -E \"0,/^([[:space:]]*)listen:[[:space:]]*[\\\"']?:8080[\\\"']?[[:space:]]*\$/s//\\1listen: \\\":80\\\"/\" /etc/rnv3/config.yaml"
fi

step "starting rnv3"
if [ "$DRY_RUN" = 0 ] && pgrep -x satdump > /dev/null; then
  echo "    a satdump process is still running — refusing to start rnv3 on top of it" >&2
  exit 1
fi
run sudo systemctl enable --now rnv3
[ "$DRY_RUN" = 0 ] && { sleep 2; systemctl --no-pager --lines=8 status rnv3 || true; }

echo
echo "==> Cutover complete. rnv3 owns the SDR; panel at http://$(hostname)/"
echo "    Watch a few passes: journalctl -u rnv3 -f"
echo "    Once satisfied, the RN2 checkout can be removed: $RN2_HOME"
