#!/system/bin/sh
# Run from an extracted Android archive; leaves endpoint and boot setup untouched.
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge-android}
export QUASAR_DATA_DIR="$state"
fail() { echo "$*" >&2; exit 1; }
test "$(id -u)" = 0 || fail 'Run the installer as root.'
case "$state" in /*) ;; *) fail 'QUASAR_DATA_DIR must be absolute.' ;; esac
case "$state" in *[!a-zA-Z0-9_./-]*|*/../*|*/..) fail 'Unsupported QUASAR_DATA_DIR path.' ;; esac
case "$(uname -m):$(cat "$package/ARCH")" in aarch64:arm64|arm64:arm64) ;; *) fail 'This experimental package requires ARM64 Android.' ;; esac
test "$("$package/bin/quasar-bridge" -v)" = dev || fail 'Android packages require a dev binary; stable Linux updater is incompatible.'
test -f "$package/bin/android-pcm" || fail 'Android PCM helper is missing from this archive.'
command -v getprop >/dev/null 2>&1 || fail 'Android getprop was not found.'
test -n "$(getprop ro.build.version.sdk)" || fail 'Android version was not reported.'
test ! -e "$state/app" || fail 'An installation exists; stop it and preserve it before replacing app/ manually.'
mkdir -p "$state"
chmod 0700 "$state"
mkdir "$state/.install-lock" 2>/dev/null || fail 'Another installation is in progress.'
stage=
cleanup() {
 result=$?
 trap - EXIT HUP INT TERM
 if [ -n "$stage" ]; then rm -rf "$stage"; fi
 rmdir "$state/.install-lock"
 exit "$result"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
stage=$(mktemp -d "$state/.install.XXXXXX")
for name in bin start.sh control.sh install.sh config.device.example.json ARCH PLATFORM VERSION LICENSE NOTICE THIRD_PARTY_NOTICES SOURCE.md docs src; do
 cp -R "$package/$name" "$stage/$name"
done
chmod 0755 "$stage/"*.sh "$stage/bin/"*
if [ ! -e "$state/config.json" ]; then cp "$stage/config.device.example.json" "$state/config.json"; fi
chmod 0600 "$state/config.json"
if [ -f "$state/runtime.env" ]; then chmod 0600 "$state/runtime.env"; fi
sh "$stage/start.sh" -check-config
mv "$stage" "$state/app"
stage=
echo "Installed experimental Android bridge in $state/app."
echo 'No endpoint change, voice-service restart, or autostart was performed.'
echo "Next: sh $state/app/control.sh start; then apply after checking its health."
