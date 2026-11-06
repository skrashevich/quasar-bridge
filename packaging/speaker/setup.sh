#!/bin/sh
# Interactive setup; no JSON parser, Python or package manager required.
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge}
export QUASAR_DATA_DIR="$state"
test "$(id -u)" = 0 || { echo 'Запустите установку от root.' >&2; exit 1; }
mkdir -p "$state"
chmod 0700 "$state"
saved=$(mktemp -d "$state/.setup.XXXXXX")
for name in config.json runtime.env; do
 if [ -e "$state/$name" ]; then cp "$state/$name" "$saved/$name"; fi
done
committed=false
hidden=false
changed=false
legacy=${QUASAR_YAM_PACKAGE_DIR:-/data/quazar/yam/data/plugins/quasar-bridge}
legacy_backup=$state/yam-package.backup
migrated=false
cleanup() {
 result=$?
 trap - EXIT HUP INT TERM
 if [ "$hidden" = true ]; then stty echo; printf '\n'; fi
 if [ "$committed" = false ] && [ "$changed" = true ]; then
  for name in config.json runtime.env; do
   if [ -e "$saved/$name" ]; then cp "$saved/$name" "$state/$name"; else rm -f "$state/$name"; fi
  done
  if [ -f "$state/app/control.sh" ]; then sh "$state/app/control.sh" restart || true; fi
 fi
 if [ "$committed" = false ] && [ "$migrated" = true ]; then
  mv "$legacy_backup" "$legacy"
  echo 'Старая установка YAM возвращена на место. При необходимости перезапустите её службу.' >&2
 fi
 rm -rf "$saved"
 exit "$result"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
ask() {
 printf '%s ' "$1"
 IFS= read -r answer || { echo 'Ввод прерван; установка отменена.' >&2; exit 1; }
}
secret() {
 printf '%s ' "$1"
 if [ -t 0 ]; then stty -echo; hidden=true; fi
 IFS= read -r answer || { echo 'Ввод прерван; установка отменена.' >&2; exit 1; }
 if [ "$hidden" = true ]; then stty echo; hidden=false; fi
 printf '\n'
}
yes() {
 case "$answer" in y|Y|yes|YES|д|Д|да|Да) return 0 ;; *) return 1 ;; esac
}
# Shell assignments, with all expansion characters escaped, including in API keys.
setting() {
 printf '%s="' "$1"
 printf '%s' "$2" | sed 's/[\\`"$]/\\&/g'
 printf '"\n'
}
save_configuration() {
 changed=true
 if [ ! -f "$state/config.json" ]; then
  cat > "$state/config.json" <<'CONFIG'
{
 "listen": "127.0.0.1:8443",
 "mode": "hybrid",
 "tls_cert": "",
 "tls_key": "",
 "allowed_ips": ["127.0.0.1", "::1"],
 "local_progress": {"enabled": true}
}
CONFIG
 fi
 {
  if [ -f "$state/runtime.env" ]; then
   sed '/^# BEGIN QUASAR SETUP$/,/^# END QUASAR SETUP$/d' "$state/runtime.env"
  fi
  printf '\n# BEGIN QUASAR SETUP\n'
  setting QUASAR_MODE hybrid
  setting QUASAR_LLM_BASE_URL "$base"
  setting QUASAR_LLM_MODEL "$model"
  setting QUASAR_LLM_API "$api"
  setting QUASAR_LLM_API_KEY "$key"
  if [ "$api" = chatgpt ]; then
   setting QUASAR_LLM_CHATGPT_AUTH_FILE "$state/chatgpt-auth.json"
  fi
  setting QUASAR_HOME_MCP_URL "$home_url"
  printf '# END QUASAR SETUP\n'
 } > "$saved/runtime.new"
 mv "$saved/runtime.new" "$state/runtime.env"
 chmod 0600 "$state/config.json" "$state/runtime.env"
}
choose_model() {
 if [ "$api" = chatgpt ]; then
  echo 'Для входа с компьютера откройте SSH-туннель во втором терминале:'
  echo 'ssh -N -L 1455:127.0.0.1:1455 -p SSH_PORT root@SPEAKER_IP'
  echo 'Замените SSH_PORT и SPEAKER_IP своими значениями. Затем откройте ссылку входа в браузере компьютера.'
  sh "$package/start.sh" -chatgpt-auth login || return 1
 fi
 echo 'Получение списка доступных моделей…'
 if ! sh "$package/start.sh" -list-models > "$saved/models"; then
  echo 'Не удалось получить список моделей. Проверьте адрес API, доступ и авторизацию.' >&2
  return 1
 fi
 if [ ! -s "$saved/models" ]; then
  echo 'API не вернул доступных моделей.' >&2
  return 1
 fi
 awk -F '\t' '{ printf "%d — %s (%s)\n", NR, $1, $2 }' "$saved/models"
 while :; do
  ask 'Номер модели:'
  case "$answer" in
   ''|*[!0-9]*) echo 'Введите номер из списка.'; continue ;;
  esac
  model=$(awk -F '\t' -v choice="$answer" 'NR == choice { print $1 }' "$saved/models")
  if [ -n "$model" ]; then return 0; fi
  echo 'Такого номера в списке нет.'
 done
}
configure=true
if [ -f "$state/config.json" ]; then
 echo 'Найдены существующие настройки. По умолчанию они будут сохранены.'
 ask 'Изменить подключение к нейросети? [y/N]'
 if ! yes; then configure=false; fi
fi
if [ "$configure" = true ]; then
 while :; do
  while :; do
   ask 'Адрес API нейросети (с /v1), chatgpt для подписки ChatGPT/Codex или claude для Anthropic:'
   case "$answer" in
    chatgpt) base=https://api.openai.com/v1; api=chatgpt; break ;;
    claude) base=https://api.anthropic.com/v1; api=chat_completions; break ;;
    http://?*|https://?*) base=$answer; api=chat_completions; break ;;
    *) echo 'Введите полный адрес, например https://llm.example.invalid/v1, или chatgpt, или claude.' ;;
   esac
  done
  key=
  if [ "$api" != chatgpt ]; then
   secret 'Ключ API (Enter, если не нужен):'
   key=$answer
   # Anthropic's OpenAI-compatible endpoint has only Chat Completions.
   if [ "${base%/}" != https://api.anthropic.com/v1 ]; then
    while :; do
     ask 'API: 1 — Chat Completions, 2 — Responses [1]:'
     case "$answer" in ''|1) api=chat_completions; break ;; 2) api=responses; break ;; esac
    done
   fi
  fi
  model=
  home_url=
  save_configuration
  if ! choose_model; then
   ask 'Ввести настройки подключения заново? [Y/n]'
   answer=${answer:-y}
   yes || exit 1
   continue
  fi
  secret 'Секретный URL Home Assistant MCP (Enter, если не нужен):'
  home_url=$answer
  save_configuration
  echo 'Проверка подключения к нейросети и Home Assistant…'
  if sh "$package/start.sh" -check-config && sh "$package/start.sh" -check-backends; then break; fi
  ask 'Проверка не прошла. Ввести настройки заново? [Y/n]'
  answer=${answer:-y}
  yes || exit 1
 done
fi
if [ -d "$legacy" ]; then
 ask 'Найдена старая установка Quasar Bridge в YAM. Перейти на самостоятельный запуск? [Y/n]'
 answer=${answer:-y}
 yes || exit 1
 if [ -e "$legacy_backup" ]; then
  echo "Уже существует $legacy_backup. Проверьте прежний переход с YAM." >&2
  exit 1
 fi
 # Disable only this plugin by moving its package out of YAM's plugin directory.
 # No YAM API, daemon shutdown or changes to other applications.
 mv "$legacy" "$legacy_backup"
 migrated=true
 # /data is a symlink on Station 2; /proc reports the physical executable path.
 legacy_executable=$(CDPATH='' cd -- "$legacy_backup" && pwd -P)/bin/quasar-bridge
 for exe in /proc/[0-9]*/exe; do
  if [ "$(readlink "$exe" 2>/dev/null)" = "$legacy_executable" ]; then
   pid=${exe%/exe}; pid=${pid##*/}
   kill "$pid"
   i=0
   while [ "$(readlink "$exe" 2>/dev/null)" = "$legacy_executable" ]; do
    i=$((i+1))
    if [ "$i" -ge 15 ]; then echo 'Старый bridge не остановился.' >&2; exit 1; fi
    sleep 1
   done
  fi
 done
fi
sh "$package/install.sh"
committed=true
if [ "$configure" = false ] && ! sh "$state/app/control.sh" check-backends; then
 echo 'Программа обновлена, но сервисы недоступны. Запустите установку повторно и измените настройки.' >&2
 exit 1
fi
if [ -s "$state/endpoint-backup.json" ]; then
 echo 'Обновление завершено. Подключение Алисы сохранено, перезагрузка не требуется.'
 exit 0
fi
ask 'Подключить Алису к Quasar Bridge? [Y/n]'
answer=${answer:-y}
if yes; then
 sh "$state/app/control.sh" apply
 ask 'Для применения подключения нужна перезагрузка. Перезагрузить сейчас? [Y/n]'
 answer=${answer:-y}
 if yes; then reboot; fi
fi
echo 'Готово. Настройки сохранены, автозапуск включён.'
