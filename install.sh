#!/bin/sh
# Run on a computer with an SSH target, or directly on an HTTPS-capable speaker.
set -eu
umask 077
release=${QUASAR_RELEASE_URL:-https://github.com/skrashevich/quasar-bridge/releases/latest/download}
pr=
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
# Flatten JSON scalars into JSON-pointer paths. Keep this bootstrap standalone:
# downloading a PR must not require jq, Python, Go or another helper script.
json_fields() {
 awk '
 function bad() { failed=1; exit 1 }
 function ws() { while (substr(doc,pos,1) ~ /[ \t\r\n]/ && pos<=length(doc)) pos++ }
 function string(    c,e,h,i,v) {
  if (substr(doc,pos++,1)!="\"") bad()
  value=""
  while (pos<=length(doc)) {
   c=substr(doc,pos++,1)
   if (c=="\"") return value
   if (c ~ /[[:cntrl:]]/) bad()
   if (c=="\\") {
    e=substr(doc,pos++,1)
    if (e=="u") {
     h=substr(doc,pos,4); if(length(h)!=4 || h ~ /[^0-9a-fA-F]/) bad()
     pos+=4; v=0
     for(i=1;i<=4;i++) v=v*16+index("0123456789abcdef",tolower(substr(h,i,1)))-1
     c=(v<128 ? sprintf("%c",v) : "\\u" h)
    } else if(e=="\"" || e=="\\" || e=="/") c=e
    else if(e=="b") c=sprintf("%c",8)
    else if(e=="f") c=sprintf("%c",12)
    else if(e=="n") c="\n"
    else if(e=="r") c="\r"
    else if(e=="t") c="\t"
    else bad()
   }
   value=value c
  }
  bad()
 }
 function emit(path,kind,text) {
  if(path !~ /[[:cntrl:]]/ && text !~ /[[:cntrl:]]/) print path "\t" kind "\t" text
 }
 function parse(path,depth,    c,key,child,i,token) {
  if(depth>64) bad()
  ws(); c=substr(doc,pos,1)
  if(c=="{") {
   pos++; ws(); if(substr(doc,pos,1)=="}") {pos++; return}
   while(1) {
    ws(); key=string()
    gsub(/~/,"~0",key); gsub(/\//,"~1",key)
    child=path "/" key
    if(seen[child]++) bad()
    ws(); if(substr(doc,pos++,1)!=":") bad()
    parse(child,depth+1); ws(); c=substr(doc,pos++,1)
    if(c=="}") return
    if(c!=",") bad()
   }
  }
  if(c=="[") {
   pos++; ws(); if(substr(doc,pos,1)=="]") {pos++; return}
   i=0
   while(1) {
    parse(path "/" i++,depth+1); ws(); c=substr(doc,pos++,1)
    if(c=="]") return
    if(c!=",") bad()
   }
  }
  if(c=="\"") {token=string(); emit(path,"string",token); return}
  if(match(substr(doc,pos),/^-?(0|[1-9][0-9]*)([.][0-9]+)?([eE][+-]?[0-9]+)?/)) {
   token=substr(doc,pos,RLENGTH); pos+=RLENGTH; emit(path,"number",token); return
  }
  for(token in literal) if(substr(doc,pos,length(token))==token) {
   pos+=length(token); emit(path,"literal",token); return
  }
  bad()
 }
 BEGIN {pos=1; literal["true"]=1; literal["false"]=1; literal["null"]=1}
 {doc=doc $0 "\n"; if(length(doc)>8388608) bad()}
 END {if(!failed) {parse("",0); ws(); if(pos<=length(doc)) bad()}}
 ' "$1"
}
json_field() {
 awk -F '\t' -v key="$2" '$1==key {value=$3; count++} END {if(count!=1) exit 1; print value}' "$1"
}
sha256_file() {
 if command -v sha256sum >/dev/null; then sha256sum "$1"
 elif command -v shasum >/dev/null; then shasum -a 256 "$1"
 else error 'Для проверки сборки нужен sha256sum или shasum.'; return 1
 fi | awk '{print $1}'
}
pr_head() {
 if ! fetch "$1/pr.json" "https://api.github.com/repos/skrashevich/quasar-bridge/pulls/$pr" ||
    ! json_fields "$1/pr.json" > "$1/pr.fields"; then
  error "Не удалось получить PR #$pr через GitHub API. Проверьте номер PR и лимит запросов GitHub."
  return 1
 fi
 json_field "$1/pr.fields" /head/sha
}
download_pr() {
 pr_work=$1
 pr_archive=$2
 command -v unzip >/dev/null || { error 'Для установки из PR нужен unzip. Запустите установщик на компьютере с unzip.'; return 1; }
 pr_sha=$(pr_head "$pr_work") || return 1
 case "$pr_sha" in ''|*[!0-9a-f]*) error 'GitHub вернул некорректный коммит PR.'; return 1 ;; esac
 test "${#pr_sha}" -eq 40 || { error 'GitHub вернул некорректный коммит PR.'; return 1; }
 info "PR #$pr · коммит $pr_sha"
 if ! fetch "$pr_work/runs.json" "https://api.github.com/repos/skrashevich/quasar-bridge/actions/workflows/ci.yml/runs?event=pull_request&head_sha=$pr_sha&per_page=100" ||
    ! json_fields "$pr_work/runs.json" > "$pr_work/runs.fields"; then
  error 'Не удалось получить сборки PR из GitHub Actions.'; return 1
 fi
 pr_run=$(awk -F '\t' -v sha="$pr_sha" -v pr="$pr" '
  $1 ~ /^\/workflow_runs\/[0-9]+\// {
   path=$1; sub(/^\/workflow_runs\//,"",path)
   i=path; sub(/\/.*/,"",i); sub(/^[^\/]+\//,"",path)
   indexes[i]=1; v[i,path]=$3
   if(path ~ /^pull_requests\/[0-9]+\/number$/ && $3==pr) member[i]=1
  }
  END {
   best=-1
   for(i in indexes) if(v[i,"head_sha"]==sha && v[i,"event"]=="pull_request" && v[i,"path"]==".github/workflows/ci.yml" && member[i] && v[i,"id"] ~ /^[1-9][0-9]*$/ && v[i,"id"]+0>best) {best=v[i,"id"]+0; chosen=i}
   if(best<0 || v[chosen,"status"]!="completed" || v[chosen,"conclusion"]!="success") exit 1
   print v[chosen,"id"]
  }' "$pr_work/runs.fields") || {
  error "У текущего коммита PR #$pr ещё нет успешной завершённой проверки. Дождитесь GitHub Actions и повторите команду."
  return 1
 }
 if ! fetch "$pr_work/artifacts.json" "https://api.github.com/repos/skrashevich/quasar-bridge/actions/runs/$pr_run/artifacts?per_page=100" ||
    ! json_fields "$pr_work/artifacts.json" > "$pr_work/artifacts.fields"; then
  error 'Не удалось получить артефакты сборки PR.'; return 1
 fi
 pr_artifact=$(awk -F '\t' -v name="speaker-release-$arch" -v run="$pr_run" -v sha="$pr_sha" '
  $1 ~ /^\/artifacts\/[0-9]+\// {
   path=$1; sub(/^\/artifacts\//,"",path)
   i=path; sub(/\/.*/,"",i); sub(/^[^\/]+\//,"",path)
   indexes[i]=1; v[i,path]=$3
  }
  END {
   count=0
   for(i in indexes) if(v[i,"name"]==name) {count++; chosen=i}
   if(count!=1 || v[chosen,"expired"]!="false" || v[chosen,"id"] !~ /^[1-9][0-9]*$/) exit 1
   if(v[chosen,"workflow_run/id"]!="" && v[chosen,"workflow_run/id"]!=run) exit 1
   if(v[chosen,"workflow_run/head_sha"]!="" && v[chosen,"workflow_run/head_sha"]!=sha) exit 1
   print v[chosen,"id"] " " v[chosen,"digest"]
  }' "$pr_work/artifacts.fields") || {
  error "Готовая сборка PR #$pr для $arch отсутствует, неоднозначна или уже истекла. Запустите проверки PR заново."
  return 1
 }
 pr_artifact_id=${pr_artifact%% *}
 pr_digest=${pr_artifact#* }
 case "$pr_digest" in sha256:*) pr_digest=${pr_digest#sha256:} ;; *) pr_digest= ;; esac
 case "$pr_digest" in ''|*[!0-9a-f]*) error 'GitHub не вернул SHA-256 артефакта.'; return 1 ;; esac
 test "${#pr_digest}" -eq 64 || { error 'GitHub вернул некорректную SHA-256 артефакта.'; return 1; }
 info "Скачивание сборки GitHub Actions #$pr_run через nightly.link…"
 if ! fetch "$pr_work/artifact.zip" "https://nightly.link/skrashevich/quasar-bridge/actions/artifacts/$pr_artifact_id.zip"; then
  error 'Не удалось скачать артефакт GitHub Actions через nightly.link. Повторите команду позже.'; return 1
 fi
 test "$(sha256_file "$pr_work/artifact.zip")" = "$pr_digest" || { error 'SHA-256 ZIP не совпадает с данными GitHub. Установка отменена.'; return 1; }
 # Read only the two expected entries; never extract arbitrary ZIP paths.
 if ! unzip -p "$pr_work/artifact.zip" "$pr_archive" > "$pr_work/$pr_archive" ||
    ! unzip -p "$pr_work/artifact.zip" "$pr_archive.sha256" > "$pr_work/$pr_archive.sha256"; then
  error 'В артефакте нет корректного архива для колонки и его контрольной суммы.'; return 1
 fi
 pr_current_sha=$(pr_head "$pr_work") || return 1
 test "$pr_current_sha" = "$pr_sha" || { error "PR #$pr изменился во время скачивания. Повторите команду для новой версии."; return 1; }
}
download_package() {
 if [ -n "$pr" ]; then download_pr "$1" "$2"
 else fetch "$1/$2" "$release/$2" && fetch "$1/$2.sha256" "$release/$2.sha256"
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
 if ! download_package "$work" "$archive"; then
  if [ -z "$pr" ]; then error 'Не удалось скачать выпуск. Проверьте подключение к интернету и повторите установку.'; fi
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
 if ! download_package "$stage" "$archive"; then
  if [ -z "$pr" ]; then error 'Не удалось скачать выпуск. Если на колонке нет HTTPS, запустите установку с компьютера по SSH.'; fi
  exit 1
 fi
 cd "$stage"
 sha256sum -c "$archive.sha256" > /dev/null || { error 'Контрольная сумма не совпала. Установка отменена.'; exit 1; }
 ok 'Архив скачан, контрольная сумма проверена.'
 mkdir package
 gzip -dc "$archive" | tar -xf - -C package
 sh "$stage/package/setup.sh"
)
usage() { printf '%s\n' 'Использование: sh install.sh [--pr НОМЕР] [root@КОЛОНКА [параметры SSH]]'; }
if [ "${1:-}" = --help ] || [ "${1:-}" = -h ]; then usage; exit 0; fi
if [ "${1:-}" = --pr ]; then
 test "$#" -ge 2 || { usage >&2; exit 1; }
 pr=$2
 case "$pr" in ''|0*|*[!0-9]*) error 'Номер PR должен быть положительным целым числом.'; exit 1 ;; esac
 if [ -n "${QUASAR_RELEASE_URL:-}" ]; then error 'Нельзя одновременно указывать --pr и QUASAR_RELEASE_URL.'; exit 1; fi
 shift 2
fi
case "${1:-}" in -*) usage >&2; exit 1 ;; esac
if [ "$#" -gt 0 ]; then host_install "$@"; else speaker_install; fi
