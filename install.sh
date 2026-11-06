#!/bin/sh
# Run on a computer with an SSH target, or directly on an HTTPS-capable speaker.
set -eu
umask 077
release=${QUASAR_RELEASE_URL:-https://github.com/skrashevich/quasar-bridge/releases/latest/download}
cyan= green= red= reset=
setup_color=0
if [ -t 1 ] && [ "${TERM:-dumb}" != dumb ] && [ -z "${NO_COLOR:-}" ]; then
 cyan=$(printf '\033[1;36m'); green=$(printf '\033[1;32m')
 red=$(printf '\033[1;31m'); reset=$(printf '\033[0m'); setup_color=1
fi
section() { printf '\n%s── %s ──%s\n\n' "$cyan" "$1" "$reset"; }
info() { printf '  %s\n' "$*"; }
ok() { printf '%s  ✓ %s%s\n' "$green" "$*" "$reset"; }
error() { printf '%s  ✗ %s%s\n' "$red" "$*" "$reset" >&2; }
# Quote each argument for both the displayed command and the remote shell.
quote() {
 case "$1" in
  ''|*[!a-zA-Z0-9_./:=@%+,-]*)
   printf "'"; printf '%s' "$1" | sed "s/'/'\\\\''/g"; printf "'" ;;
  *) printf '%s' "$1" ;;
 esac
}
fetch() {
 if command -v curl >/dev/null; then
  curl -fsSL --connect-timeout 15 -o "$1" "$2"
 elif command -v wget >/dev/null; then
  wget -q -O "$1" "$2"
 else
  error 'Для скачивания нужен curl или wget.'; exit 1
 fi
}
architecture() {
 case "$1" in
  aarch64|arm64) echo arm64 ;;
  armv7l|armv8l) echo armv7 ;;
  *) echo 'Поддерживаются только Linux ARM64 и ARMv7.' >&2; exit 1 ;;
 esac
}
host_install() (
 target=$1
 shift
 tunnel='ssh -N -L 1455:127.0.0.1:1455'
 for argument in "$@" "$target"; do tunnel="$tunnel $(quote "$argument")"; done
 work=$(mktemp -d /tmp/quasar-bridge-ssh.XXXXXX)
 remote_stage=
 # Reuse authentication, including password login, across file transfers.
 set -- -o ControlMaster=auto -o ControlPersist=60 -o "ControlPath=$work/ssh" "$@"
 # shellcheck disable=SC2329
 cleanup() {
  result=$?
  trap - EXIT HUP INT TERM
  if [ -n "$remote_stage" ]; then
   ssh -n "$@" "$target" "rm -rf '$remote_stage'" >/dev/null 2>&1 || true
  fi
  ssh -O exit "$@" "$target" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$result"
 }
 trap 'cleanup "$@"' EXIT
 trap 'exit 1' HUP INT TERM
 # Keep the wizard interactive even when this script is piped into sh.
 if [ -t 0 ] || [ -f "$0" ]; then exec 3<&0; else exec 3</dev/tty; fi
 section 'Подготовка установки'
 info "Подключение к ${target}…"
 arch=$(ssh -n "$@" "$target" 'uname -m')
 arch=$(architecture "$arch")
 ok "Колонка подключена · $arch"
 archive=quasar-bridge-linux-$arch.tar.gz
 info "Скачивание Quasar Bridge для ${arch}…"
 if ! fetch "$work/$archive" "$release/$archive" ||
    ! fetch "$work/$archive.sha256" "$release/$archive.sha256"; then
  error 'Не удалось скачать выпуск. Проверьте подключение к интернету и повторите установку.'
  exit 1
 fi
 if ! (
  cd "$work"
  if command -v sha256sum >/dev/null; then sha256sum -c "$archive.sha256" > /dev/null
  else shasum -a 256 -c "$archive.sha256" > /dev/null
  fi
 ); then
  error 'Контрольная сумма не совпала. Установка отменена; скачайте выпуск заново.'
  exit 1
 fi
 ok 'Архив скачан, контрольная сумма проверена.'
 # Keep the compressed archive and extraction off the small persistent data volume.
 remote_stage=$(ssh -n "$@" "$target" 'umask 077; mktemp -d /tmp/quasar-bridge-install.XXXXXX')
 case "$remote_stage" in
  /tmp/quasar-bridge-install.*) ;;
  *) echo 'Колонка не вернула каталог установки.' >&2; remote_stage=; exit 1 ;;
 esac
 case "$remote_stage" in
  *[!a-zA-Z0-9_./-]*) echo 'Некорректный путь установки.' >&2; remote_stage=; exit 1 ;;
 esac
 for file in "$archive" "$archive.sha256"; do
  ssh -T "$@" "$target" "umask 077; cat > '$remote_stage/$file'" < "$work/$file"
 done
 ok 'Файлы переданы на колонку.'
 ssh -tt "$@" "$target" "set -eu; cd '$remote_stage'; sha256sum -c '$archive.sha256' > /dev/null; mkdir package; gzip -dc '$archive' | tar -xf - -C package; QUASAR_SETUP_COLOR=$setup_color QUASAR_SSH_TUNNEL=$(quote "$tunnel") sh package/setup.sh" <&3
)
speaker_install() (
 test "$(id -u)" = 0 || { echo 'Запустите установку от root.' >&2; exit 1; }
 arch=$(architecture "$(uname -m)")
 for command in sha256sum gzip tar mktemp; do
  command -v "$command" >/dev/null || { echo "Не найдена утилита: $command" >&2; exit 1; }
 done
 archive=quasar-bridge-linux-$arch.tar.gz
 stage=$(mktemp -d /tmp/quasar-bridge-download.XXXXXX)
 trap 'rm -rf "$stage"' EXIT
 trap 'exit 1' HUP INT TERM
 section 'Подготовка установки'
 info "Скачивание Quasar Bridge для ${arch}…"
 if ! fetch "$stage/$archive" "$release/$archive" ||
    ! fetch "$stage/$archive.sha256" "$release/$archive.sha256"; then
  error 'Не удалось скачать выпуск. Если на колонке нет HTTPS, запустите установку с компьютера по SSH.'
  exit 1
 fi
 cd "$stage"
 sha256sum -c "$archive.sha256" > /dev/null || { error 'Контрольная сумма не совпала. Установка отменена.'; exit 1; }
 ok 'Архив скачан, контрольная сумма проверена.'
 mkdir package
 gzip -dc "$archive" | tar -xf - -C package
 sh "$stage/package/setup.sh"
)
if [ "$#" -gt 0 ]; then host_install "$@"; else speaker_install; fi
