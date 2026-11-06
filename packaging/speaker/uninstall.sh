#!/bin/sh
# Keep user state; never leave Alice pointing at a removed bridge.
set -eu
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge}
init=${QUASAR_INIT_DIR:-/etc/init.d}
export QUASAR_DATA_DIR="$state"
test "$(id -u)" = 0 || { echo 'Run as root.' >&2; exit 1; }
if [ -e "$state/endpoint-backup.json" ]; then
 echo 'Run control.sh rollback and reboot before uninstalling.' >&2
 exit 1
fi
mkdir "$state/.install-lock" 2>/dev/null || { echo 'Another installation operation is in progress.' >&2; exit 1; }
# shellcheck source=packaging/speaker/firmware.sh
. "$package/firmware.sh"
trap 'firmware_restore; rmdir "$state/.install-lock"' EXIT
trap 'exit 1' HUP INT TERM
sh "$package/control.sh" stop
hook=$init/S96quasar-bridge
if [ -e "$hook" ]; then
 grep -q 'quasar-bridge standalone init hook' "$hook" || { echo 'Unrelated boot hook; inspect it first.' >&2; exit 1; }
 firmware_writable
 rm "$hook"
 firmware_restore
fi
rm -rf "$state/app" "$state/app.previous"
echo "Uninstalled. Settings and logs retained in $state."
