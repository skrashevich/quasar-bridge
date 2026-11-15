#!/system/bin/sh
# Manual service and reversible firmware-config overlay. No boot hook is installed.
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge-android}
export QUASAR_DATA_DIR="$state"
pidfile=$state/bridge.pid
lock=$state/service.lock
health=http://127.0.0.1:8443/healthz
record=$state/endpoint-mount
copy=$state/speaker-config.json
backup=$state/endpoint-backup.json
fail() { echo "$*" >&2; exit 1; }
valid_path() {
 case "$1" in /*) ;; *) return 1 ;; esac
 case "$1" in *[!a-zA-Z0-9_./-]*|*/../*|*/..) return 1 ;; esac
}
valid_path "$state" || fail 'Unsupported QUASAR_DATA_DIR path.'
healthy() { "$package/bin/quasar-bridge" -healthcheck "$health" >/dev/null 2>&1; }
healthy_backend() { "$package/bin/quasar-bridge" -healthcheck "$health" -healthcheck-android-backend >/dev/null 2>&1; }
running() {
 test -s "$pidfile" || return 1
 pid=$(cat "$pidfile")
 case "$pid" in ''|*[!0-9]*) return 1 ;; esac
 test "$pid" -gt 1 || return 1
 test "$(readlink "/proc/$pid/exe" 2>/dev/null)" = "$package/bin/quasar-bridge" && kill -0 "$pid" 2>/dev/null
}
# 0: mounted, 1: absent, 2: inspection failed. Never collapse failures into
# absence: rollback must retain its recovery files if the table cannot be read.
mounted() {
 test -f /proc/self/mountinfo && test -r /proc/self/mountinfo || return 2
 exec 3< /proc/self/mountinfo || return 2
 mount_seen=false
 while :; do
  mount_id= mount_parent= mount_device= mount_root= mount_target= mount_options= mount_rest=
  if IFS=' ' read -r mount_id mount_parent mount_device mount_root mount_target mount_options mount_rest <&3; then
   case "$mount_id:$mount_parent" in *[!0-9:]*|:*|*:) exec 3<&-; return 2 ;; esac
   case "$mount_target" in /*) ;; *) exec 3<&-; return 2 ;; esac
   case "$mount_rest" in '- '*|*' - '*) ;; *) exec 3<&-; return 2 ;; esac
   mount_seen=true
   if [ "$mount_target" = "$1" ]; then exec 3<&-; return 0; fi
  else
   mount_read_result=$?
   exec 3<&-
   # A complete proc table is nonempty and ends at a line boundary.
   test "$mount_read_result" = 1 && test "$mount_seen" = true && test -z "$mount_id$mount_parent$mount_device$mount_root$mount_target$mount_options$mount_rest" || return 2
   return 1
  fi
 done
}
identity() { stat -c '%d:%i' "$1"; }
start() {
 if running; then healthy; return; fi
 if healthy; then fail 'Port 8443 serves an unmanaged bridge; stop it first.'; fi
 sh "$package/start.sh" -check-config
 if [ -f "$state/service.log" ] && [ "$(wc -c < "$state/service.log")" -gt 1048576 ]; then
  mv -f "$state/service.log" "$state/service.log.1"
 fi
 nohup sh "$package/start.sh" >> "$state/service.log" 2>&1 < /dev/null &
 echo "$!" > "$pidfile"
 i=0
 while [ "$i" -lt 15 ]; do
  sleep 1
  if running && healthy; then echo 'Android bridge started and healthy'; return; fi
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
apply() {
 running && healthy && healthy_backend || fail 'Start this bridge with the Android backend relay before apply.'
 test ! -e "$record" || fail 'An endpoint overlay record exists; run rollback first.'
 target=$(readlink -f "${QUASAR_DEVICE_CONFIG:-/system/vendor/quasar/quasar.cfg}")
 valid_path "$target" || fail 'Unsupported resolved firmware path.'
 test -f "$target" || fail 'No firmware configuration file.'
 if mounted "$target"; then
  fail 'The firmware target already has a mount; inspect it first.'
 else
  mount_result=$?
  test "$mount_result" = 1 || fail 'Cannot inspect firmware mounts; leaving endpoint state untouched.'
 fi
 # A failed previous patch may leave its backup; never reuse it silently.
 test ! -e "$backup" || fail 'A previous endpoint backup exists; inspect it first.'
 cp "$target" "$copy"
 chmod 0600 "$copy"
 "$package/bin/quasar-bridge" -speaker-endpoint apply -speaker-android -speaker-config "$copy" -speaker-backup "$backup"
 # The overlay is read by native services that may run under an Android app UID.
 # Only this non-secret firmware config is made readable; state stays root-only.
 chmod 0644 "$copy"
 printf '%s\n%s\n' "$target" "$(identity "$copy")" > "$record.new"
 mv "$record.new" "$record"
 # Keep the record if mount fails: rollback can safely discard a stale record.
 mount -o bind "$copy" "$target"
 mounted "$target" && test "$(identity "$target")" = "$(identity "$copy")" || fail 'Cannot verify the endpoint overlay; inspect mounts before proceeding.'
 echo 'Temporary endpoint overlay applied. Restart the native voice service manually.'
 echo 'The vendor file was not rewritten. The overlay disappears on reboot.'
}
rollback() {
 test -f "$record" || fail 'No endpoint overlay record.'
 target=$(sed -n '1p' "$record")
 saved=$(sed -n '2p' "$record")
 valid_path "$target" || fail 'Invalid endpoint overlay record.'
 case "$saved" in ''|*[!0-9:]*|:*|*:) fail 'Invalid overlay identity.' ;; esac
 if mounted "$target"; then
  test -f "$copy" && test "$(identity "$copy")" = "$saved" && test "$(identity "$target")" = "$saved" || fail 'Endpoint mount is no longer this overlay; refusing unmount.'
  umount "$target"
  if mounted "$target"; then
   fail 'Another mount remains at the firmware target; inspect it first.'
  else
   mount_result=$?
   test "$mount_result" = 1 || fail 'Cannot inspect firmware mounts after unmount; keeping recovery files.'
  fi
 else
  mount_result=$?
  test "$mount_result" = 1 || fail 'Cannot inspect firmware mounts; keeping recovery files.'
  echo 'Endpoint overlay is already absent (for example, after reboot).'
 fi
 "$package/bin/quasar-bridge" -speaker-android-cache-rollback "${QUASAR_NATIVE_CONFIG_STORAGE:-/data/quasar/data/user_config.dat}" \
  -speaker-config "$target" -speaker-backup "$backup"
 rm -f "$record" "$backup" "$copy"
 echo 'Original firmware endpoint restored. Restart the native voice service manually.'
}
action=${1:-status}
case "$action" in
 check) exec sh "$package/start.sh" -check-config ;;
 check-backends) exec sh "$package/start.sh" -check-backends ;;
 status) running && healthy || exit 1; echo "Android bridge healthy (PID $pid)"; exit 0 ;;
 logs) exec tail -n 100 "$state/service.log" ;;
 start|stop|restart|apply|rollback)
  test "$(id -u)" = 0 || fail 'Run this operation as root.'
  mkdir -p "$state"
  mkdir "$lock" 2>/dev/null || fail "Service operation locked ($lock); inspect a stale lock after interruption."
  trap 'rmdir "$lock"' EXIT
  trap 'exit 1' HUP INT TERM
  case "$action" in
   start) start ;;
   stop) stop ;;
   restart) sh "$package/start.sh" -check-config; stop; start ;;
   apply) apply ;;
   rollback) rollback ;;
  esac
  ;;
 *) fail 'usage: control.sh start|stop|restart|status|logs|check|check-backends|apply|rollback' ;;
esac
