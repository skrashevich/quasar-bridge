#!/bin/sh
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge}
export QUASAR_DATA_DIR="$state"
pidfile=$state/bridge.pid
# BusyBox firmware clears /tmp at boot; a power loss must not leave autostart locked.
lock=/tmp/quasar-bridge-service.lock
health=http://127.0.0.1:8443/healthz
fail() { echo "$*" >&2; exit 1; }
healthy() { wget -q -T 2 -t 1 -O /dev/null "$health" 2>/dev/null; }
running() {
 test -s "$pidfile" || return 1
 pid=$(cat "$pidfile")
 case "$pid" in ''|*[!0-9]*) return 1 ;; esac
 test "$pid" -gt 1 || return 1
 # Never signal an unrelated process after a stale PID file or PID reuse.
 test "$(readlink "/proc/$pid/exe" 2>/dev/null)" = "$package/bin/quasar-bridge" && kill -0 "$pid" 2>/dev/null
}
start() {
 if running; then healthy; return; fi
 if healthy; then fail 'Port 8443 already serves another bridge; stop its old launcher first.'; fi
 sh "$package/start.sh" -check-config
 if [ -f "$state/service.log" ] && [ "$(wc -c < "$state/service.log")" -gt 1048576 ]; then
  mv -f "$state/service.log" "$state/service.log.1"
 fi
 nohup "$package/start.sh" >> "$state/service.log" 2>&1 < /dev/null &
 echo "$!" > "$pidfile"
 i=0
 while [ "$i" -lt 15 ]; do
  sleep 1
  if running && healthy; then echo 'Bridge started and healthy'; return; fi
  kill -0 "$(cat "$pidfile")" 2>/dev/null || break
  i=$((i+1))
 done
 stop
 fail "Bridge did not become ready; inspect $state/service.log"
}
stop() {
 if running; then
  kill "$pid"
  i=0
  while running; do
   i=$((i+1)); test "$i" -lt 15 || fail 'Bridge did not stop; keeping PID file.'
   sleep 1
  done
 fi
 rm -f "$pidfile"
}
action=${1:-status}
case "$action" in
 check) exec sh "$package/start.sh" -check-config ;;
 check-backends) exec sh "$package/start.sh" -check-backends ;;
 chatgpt-login|chatgpt-status|chatgpt-models|chatgpt-logout)
  exec sh "$package/start.sh" -chatgpt-auth "${action#chatgpt-}"
  ;;
 status) running && healthy || exit 1; echo "Bridge healthy (PID $pid)"; exit 0 ;;
 logs) exec tail -n 100 "$state/service.log" ;;
 start|stop|restart)
  mkdir "$lock" 2>/dev/null || fail "Service operation in progress ($lock)."
  trap 'rmdir "$lock"' EXIT
  trap 'exit 1' HUP INT TERM
  case "$action" in
   start) start ;;
   stop) stop ;;
   restart) sh "$package/start.sh" -check-config; stop; start ;;
  esac
  ;;
 apply|rollback)
  if [ "$action" = apply ]; then
   if ! running || ! healthy; then
    fail 'Start this bridge and check its health before apply.'
   fi
  else
   test -s "$state/endpoint-backup.json" || fail 'No endpoint backup; nothing to restore.'
  fi
  # shellcheck source=packaging/speaker/firmware.sh
  . "$package/firmware.sh"
  trap 'firmware_restore' EXIT
  trap 'exit 1' HUP INT TERM
  firmware_writable
  "$package/bin/quasar-bridge" -speaker-endpoint "$action" \
   -speaker-config "${QUASAR_DEVICE_CONFIG:-/system/vendor/quasar/quasar.cfg}" \
   -speaker-backup "$state/endpoint-backup.json"
  firmware_restore
  trap - EXIT HUP INT TERM
  echo 'Endpoint saved. Reboot the speaker to apply it.'
  ;;
 *) fail 'usage: control.sh start|stop|restart|status|logs|check|check-backends|chatgpt-login|chatgpt-status|chatgpt-models|chatgpt-logout|apply|rollback' ;;
esac
