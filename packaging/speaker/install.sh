#!/bin/sh
# Run from an extracted, verified release archive on the speaker, as root.
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge}
init=${QUASAR_INIT_DIR:-/etc/init.d}
export QUASAR_DATA_DIR="$state"
fail() { echo "$*" >&2; exit 1; }
test "$(id -u)" = 0 || fail 'Run the installer as root.'
case "$state" in /*) ;; *) fail 'QUASAR_DATA_DIR must be absolute.' ;; esac
case "$state" in *[!a-zA-Z0-9_./-]*|*/../*|*/..) fail 'Unsupported QUASAR_DATA_DIR path.' ;; esac
case "$(uname -m):$(cat "$package/ARCH")" in
 aarch64:arm64|arm64:arm64|armv7l:armv7|armv8l:armv7) ;;
 *) fail 'Archive architecture does not match this speaker.' ;;
esac
test -d "$init" || fail 'No init.d directory; this firmware needs a different boot hook.'
hook=$init/S96quasar-bridge
if [ -e "$hook" ] && ! grep -q 'quasar-bridge standalone init hook' "$hook"; then
 fail 'An unrelated S96quasar-bridge hook exists; inspect it first.'
fi
mkdir -p "$state"
chmod 0700 "$state"
mkdir "$state/.install-lock" 2>/dev/null || fail 'Another install/uninstall is in progress.'
# shellcheck source=packaging/speaker/firmware.sh
. "$package/firmware.sh"
stage=
switched=false
had_old=false
cleanup() {
 result=$?
 trap - EXIT HUP INT TERM
 if [ "$result" -ne 0 ] && [ "$switched" = true ]; then
  if [ ! -d "$state/app" ] || sh "$state/app/control.sh" stop; then
   rm -rf "$state/app"
   if [ "$had_old" = true ]; then
    mv "$state/app.previous" "$state/app"
    sh "$state/app/control.sh" start || echo 'Previous version could not restart; inspect service.log.' >&2
   fi
  else
   echo 'Could not stop candidate; leaving app and app.previous for manual recovery.' >&2
  fi
 fi
 if [ -n "$stage" ]; then rm -rf "$stage"; fi
 firmware_restore || result=1
 rmdir "$state/.install-lock"
 exit "$result"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
stage=$(mktemp -d "$state/.install.XXXXXX")
for name in bin start.sh control.sh install.sh setup.sh uninstall.sh firmware.sh S96quasar-bridge config.device.example.json ARCH VERSION LICENSE NOTICE THIRD_PARTY_NOTICES SOURCE.md docs; do
 cp -R "$package/$name" "$stage/$name"
done
chmod 0755 "$stage/"*.sh "$stage/bin/quasar-bridge"
if [ ! -e "$state/config.json" ]; then
 cp "$stage/config.device.example.json" "$state/config.json"
fi
chmod 0600 "$state/config.json"
if [ -f "$state/runtime.env" ]; then chmod 0600 "$state/runtime.env"; fi
# Validate the candidate against existing state before stopping the working version.
sh "$stage/start.sh" -check-config
if [ -d "$state/app" ]; then
 sh "$state/app/control.sh" stop
 had_old=true
fi
if wget -q -T 2 -t 1 -O /dev/null http://127.0.0.1:8443/healthz 2>/dev/null; then
 fail 'An unmanaged bridge is still running. Stop the YAM/manual instance and disable its autostart first.'
fi
rm -rf "$state/app.previous"
if [ "$had_old" = true ]; then mv "$state/app" "$state/app.previous"; fi
switched=true
mv "$stage" "$state/app"
stage=
sh "$state/app/control.sh" start
# Publish the boot hook only after the new listener is healthy.
firmware_writable
sed "s|@STATE@|$state|" "$state/app/S96quasar-bridge" > "$hook.new"
chmod 0755 "$hook.new"
mv -f "$hook.new" "$hook"
firmware_restore
echo "Installed $(cat "$state/app/VERSION"). Autostart: $hook"
echo "Config: $state/config.json. Next: control.sh check-backends, then control.sh apply."
