#!/system/bin/sh
# Experimental rooted Android launcher; invoke with /system/bin/sh.
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge-android}
case "$state" in /*) ;; *) echo 'QUASAR_DATA_DIR must be absolute.' >&2; exit 1 ;; esac
export QUASAR_DATA_DIR="$state"
if [ -f "$state/runtime.env" ]; then
 set -a
 . "$state/runtime.env"
 set +a
fi
if [ -z "${QUASAR_DNS_SERVER:-}" ]; then
 QUASAR_DNS_SERVER=$(getprop net.dns1)
 if [ -z "$QUASAR_DNS_SERVER" ]; then QUASAR_DNS_SERVER=$(getprop net.dns2); fi
fi
if [ -z "$QUASAR_DNS_SERVER" ]; then
 echo 'Android DNS was not reported; set QUASAR_DNS_SERVER in runtime.env.' >&2
 exit 1
fi
export QUASAR_DNS_SERVER
export SSL_CERT_FILE="${SSL_CERT_FILE:-/system/vendor/quasar/ca-certificates.crt}"
export TMPDIR="$state/tmp"
export GOMAXPROCS="${GOMAXPROCS:-1}"
export GOMEMLIMIT="${GOMEMLIMIT:-64MiB}"
mkdir -p "$TMPDIR"
cd "$state"
exec "$package/bin/quasar-bridge" -config "$state/config.json" "$@"
