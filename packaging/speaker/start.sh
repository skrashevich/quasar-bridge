#!/bin/sh
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge}
if [ -f "$state/runtime.env" ]; then
 set -a
 # shellcheck disable=SC1091
 . "$state/runtime.env"
 set +a
fi
export SSL_CERT_FILE="${SSL_CERT_FILE:-/system/vendor/quasar/ca-certificates.crt}"
export GOMAXPROCS="${GOMAXPROCS:-1}"
export GOMEMLIMIT="${GOMEMLIMIT:-64MiB}"
cd "$state"
exec "$package/bin/quasar-bridge" -config "$state/config.json" "$@"
