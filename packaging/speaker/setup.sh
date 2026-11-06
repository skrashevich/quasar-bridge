#!/bin/sh
# Interactive setup; no JSON parser, Python or package manager required.
set -eu
umask 077
package=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge}
export QUASAR_DATA_DIR="$state"
bold= cyan= green= yellow= red= reset=
if [ -t 1 ] && [ "${TERM:-dumb}" != dumb ] && [ -z "${NO_COLOR:-}" ] && [ "${QUASAR_SETUP_COLOR:-1}" != 0 ]; then
 bold=$(printf '\033[1m'); cyan=$(printf '\033[1;36m'); green=$(printf '\033[1;32m')
 yellow=$(printf '\033[1;33m'); red=$(printf '\033[1;31m'); reset=$(printf '\033[0m')
fi
section() { printf '\n%s── %s ──%s\n\n' "$cyan" "$1" "$reset"; }
info() { printf '  %s\n' "$*"; }
ok() { printf '%s  ✓ %s%s\n' "$green" "$*" "$reset"; }
warn() { printf '%s  ! %s%s\n' "$yellow" "$*" "$reset"; }
error() { printf '%s  ✗ %s%s\n' "$red" "$*" "$reset" >&2; }
test "$(id -u)" = 0 || { error 'Запустите установку от root.'; exit 1; }
mkdir -p "$state"
chmod 0700 "$state"
saved=$(mktemp -d "$state/.setup.XXXXXX")
for name in config.json runtime.env; do
 if [ -e "$state/$name" ]; then cp "$state/$name" "$saved/$name"; fi
done
committed=false
hidden=false
changed=false
configure_home=true
home_collected=false
home_url=
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
  warn 'Прежние настройки восстановлены.' >&2
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
 printf '%s? %s%s ' "$bold" "$1" "$reset"
 IFS= read -r answer || { error 'Ввод прерван; установка отменена.'; exit 1; }
 if [ ! -t 0 ]; then printf '\n'; fi
}
secret() {
 if [ -t 0 ]; then stty -echo; hidden=true; fi
 printf '%s? %s%s ' "$bold" "$1" "$reset"
 IFS= read -r answer || { error 'Ввод прерван; установка отменена.'; exit 1; }
 if [ "$hidden" = true ]; then stty echo; hidden=false; fi
 printf '\n'
}
yes() {
 case "$answer" in y|Y|yes|YES|д|Д|да|Да) return 0 ;; *) return 1 ;; esac
}
# Successful commands stay concise; failures retain their complete diagnostics.
run_step() {
 label=$1
 shift
 info "${label}…"
 if "$@" > "$saved/command.log" 2>&1; then
  ok "$label"
 else
  error "$label — ошибка."
  cat "$saved/command.log" >&2
  return 1
 fi
}
summary() {
 section 'Готово'
 version=
 if [ -f "$package/VERSION" ]; then version=" $(cat "$package/VERSION")"; fi
 ok "Quasar Bridge$version установлен и запущен."
 info 'Автозапуск включён.'
 info "Настройки: $state/config.json и $state/runtime.env"
 info "Журнал: $state/service.log"
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
   if [ "$configure_home" = true ]; then
    sed '/^# BEGIN QUASAR SETUP$/,/^# END QUASAR SETUP$/d' "$state/runtime.env"
   else
    # Keep HA assignments at their original position relative to manual
    # overrides. Do not evaluate or reconstruct their shell-escaped values.
    sed '/^# BEGIN QUASAR SETUP$/,/^# END QUASAR SETUP$/ {
     /^QUASAR_HOME_MCP_URL=/!d
    }' "$state/runtime.env"
   fi
  fi
  printf '\n# BEGIN QUASAR SETUP\n'
  setting QUASAR_MODE hybrid
  setting QUASAR_LLM_BASE_URL "$base"
  setting QUASAR_LLM_MODEL "$model"
  setting QUASAR_LLM_API "$api"
  setting QUASAR_LLM_API_KEY "$key"
  setting QUASAR_LLM_CHAT_PARAMETERS "$chat_parameters"
  if [ "$api" = chatgpt ]; then
   setting QUASAR_LLM_CHATGPT_AUTH_FILE "$state/chatgpt-auth.json"
  fi
  if [ "$configure_home" = true ]; then
   setting QUASAR_HOME_MCP_URL "$home_url"
  fi
  printf '# END QUASAR SETUP\n'
 } > "$saved/runtime.new"
 mv "$saved/runtime.new" "$state/runtime.env"
 chmod 0600 "$state/config.json" "$state/runtime.env"
}
save_soundcloud_proxy() {
 changed=true
 {
  if [ -f "$state/runtime.env" ]; then
   sed '/^# BEGIN QUASAR SOUNDCLOUD PROXY$/,/^# END QUASAR SOUNDCLOUD PROXY$/d' "$state/runtime.env"
  fi
  printf '\n# BEGIN QUASAR SOUNDCLOUD PROXY\n'
  setting QUASAR_SOUNDCLOUD_PROXY_URL "$soundcloud_proxy"
  printf '# END QUASAR SOUNDCLOUD PROXY\n'
 } > "$saved/runtime.proxy.new"
 chmod 0600 "$saved/runtime.proxy.new"
 mv "$saved/runtime.proxy.new" "$state/runtime.env"
}
choose_model() {
 if [ "$api" = chatgpt ]; then
  section 'Вход в ChatGPT'
  info '1. Откройте второй терминал на компьютере и выполните:'
  printf '\n%s  %s%s\n\n' "$bold" "${QUASAR_SSH_TUNNEL:-ssh -N -L 1455:127.0.0.1:1455 -p SSH_PORT root@SPEAKER_IP}" "$reset"
  if [ -z "${QUASAR_SSH_TUNNEL:-}" ]; then
   info 'Замените SSH_PORT и SPEAKER_IP портом SSH и адресом колонки.'
  fi
  info 'Оставьте второй терминал открытым до завершения входа.'
  info '2. Откройте ссылку ниже в браузере компьютера и войдите в аккаунт.'
  printf '\n'
  rm -f "$saved/login-ok"
  # Shell printf streams each line even when output is piped to tee.
  # Keep the login exit status on shells without pipefail.
  (sh "$package/start.sh" -chatgpt-auth login && : > "$saved/login-ok") |
   while IFS= read -r auth_line || [ -n "$auth_line" ]; do
    case "$auth_line" in
     '{"signed_in":'*) ;;
     'Continue with ChatGPT: open this URL in your browser.') info 'Ссылка для входа:' ;;
     'Waiting for browser sign-in (10 minutes)…') info 'Ожидание входа в браузере (до 10 минут)…' ;;
     'ChatGPT sign-in completed. Usage and app limits: '*) info 'Лимиты подписки: https://chatgpt.com/settings/usage' ;;
     https://*) printf '\n%s%s%s\n\n' "$bold" "$auth_line" "$reset" ;;
     *) printf '%s\n' "$auth_line" ;;
    esac
   done
  if [ ! -f "$saved/login-ok" ]; then
   error 'Не удалось войти в ChatGPT.'
   return 1
  fi
  ok 'Вход в ChatGPT выполнен. Второй терминал можно закрыть.'
 fi
 section 'Выбор модели'
 info 'Получение доступных моделей…'
 if ! sh "$package/start.sh" -list-models > "$saved/models"; then
  error 'Не удалось получить список моделей. Проверьте адрес API, доступ и авторизацию.'
  return 1
 fi
 if [ ! -s "$saved/models" ]; then
  error 'API не вернул доступных моделей.'
  return 1
 fi
 printf '\n'
 awk -F '\t' '{ name = $2; sub(/^"/, "", name); sub(/"$/, "", name); printf "  %d — %s (%s)\n", NR, $1, name }' "$saved/models"
 printf '\n'
 while :; do
  ask 'Номер модели:'
  case "$answer" in
   ''|*[!0-9]*) warn 'Введите номер из списка.'; continue ;;
  esac
  model=$(awk -F '\t' -v choice="$answer" 'NR == choice { print $1 }' "$saved/models")
  if [ -n "$model" ]; then ok "Выбрана модель: $model"; return 0; fi
  warn 'Такого номера в списке нет.'
 done
}
probe_compatibility() {
 info 'Подбор совместимых параметров API…'
 if sh "$package/start.sh" -probe-llm-compat > "$saved/parameters" &&
    [ -s "$saved/parameters" ]; then
  chat_parameters=$(cat "$saved/parameters")
  ok 'Совместимые параметры API подобраны.'
  if [ "$chat_parameters" = '{}' ]; then
   info 'Подходят стандартные параметры.'
  else
   info "$chat_parameters"
  fi
 else
  error 'Не удалось подобрать совместимые параметры API.'
  return 1
 fi
}
configure=true
section 'Quasar Bridge · установка и обновление'
info 'Enter выбирает вариант в квадратных скобках. Ctrl+C отменяет установку.'
if [ -f "$state/config.json" ]; then
 configure_home=false
 printf '\n'
 ok 'Найдены существующие настройки. По умолчанию они будут сохранены.'
 ask 'Изменить подключение к нейросети? [y/N]'
 if ! yes; then configure=false; fi
fi
if [ "$configure" = true ]; then
 if [ "$configure_home" = false ]; then
  info 'Подключение Home Assistant сохранится без изменений.'
 fi
 while :; do
  section 'Подключение к нейросети'
  info 'Укажите адрес API с /v1 или введите chatgpt для подписки ChatGPT/Codex.'
  printf '\n'
  while :; do
   ask 'Адрес API нейросети или chatgpt:'
   case "$answer" in
    chatgpt) base=https://api.openai.com/v1; api=chatgpt; break ;;
    http://?*|https://?*) base=$answer; api=chat_completions; break ;;
    *) warn 'Введите полный адрес, например https://llm.example.invalid/v1, или chatgpt.' ;;
   esac
  done
  key=
  if [ "$api" != chatgpt ]; then
   secret 'Ключ API (Enter, если не нужен):'
   key=$answer
   printf '\n'
   info '1 — Chat Completions (по умолчанию)'
   info '2 — Responses'
   while :; do
    ask 'Тип API [1]:'
    case "$answer" in
     ''|1) api=chat_completions; break ;;
     2) api=responses; break ;;
     *) warn 'Введите 1 или 2.' ;;
    esac
   done
  fi
  model=
  # A different provider or API must not inherit the previous compatibility profile.
  chat_parameters='{}'
  save_configuration
  if ! choose_model; then
   ask 'Ввести настройки подключения заново? [Y/n]'
   answer=${answer:-y}
   yes || exit 1
   continue
  fi
  save_configuration
  if [ "$api" = chat_completions ] && ! probe_compatibility; then
   ask 'Ввести настройки подключения заново? [Y/n]'
   answer=${answer:-y}
   yes || exit 1
   continue
  fi
  if [ "$configure_home" = true ] && [ "$home_collected" = false ]; then
   section 'Home Assistant · умный дом'
   info 'Вставьте полный URL из ha-mcp. Ввод виден, чтобы можно было проверить адрес.'
   ask 'Секретный URL Home Assistant MCP (Enter, если не нужен):'
   home_url=$answer
   home_collected=true
  fi
  save_configuration
  while :; do
   section 'Проверка подключений'
   if run_step 'Проверка настроек' sh "$package/start.sh" -check-config &&
      run_step 'Проверка нейросети и Home Assistant' sh "$package/start.sh" -check-backends; then break 2; fi
   printf '\n'
   info '1 — Изменить подключение к нейросети (по умолчанию)'
   info '2 — Изменить URL Home Assistant'
   info '3 — Повторить проверку'
   info '0 — Отменить установку'
   ask 'Что сделать? [1]:'
   case "$answer" in
    ''|1|y|Y|yes|YES|д|Д|да|Да) break ;;
    2)
     section 'Home Assistant · исправление подключения'
     ask 'Секретный URL Home Assistant MCP (Enter, чтобы отключить умный дом):'
     home_url=$answer
     configure_home=true
     home_collected=true
     save_configuration
     ;;
    3) ;;
    0|n|N|no|NO|н|Н|нет|Нет) exit 1 ;;
    *) warn 'Введите 1, 2, 3 или 0.' ;;
   esac
  done
 done
fi
echo 'SoundCloud: публичный каталог работает без учётных данных.'
echo '0 — сохранить настройки (при новой установке выключен), 1 — выключить,'
echo '2 — только явные запросы SoundCloud, 3 — SoundCloud по умолчанию,'
echo '4 — сравнивать с Яндексом (при равенстве Яндекс), 5 — сравнивать (при равенстве SoundCloud).'
while :; do
 ask 'Режим SoundCloud [0]:'
 case "$answer" in
  ''|0) break ;;
  1) soundcloud_mode=off ;;
  2) soundcloud_mode=explicit ;;
  3) soundcloud_mode=default ;;
  4) soundcloud_mode=compare-yandex ;;
  5) soundcloud_mode=compare-soundcloud ;;
  *) echo 'Введите число от 0 до 5.'; continue ;;
 esac
 changed=true
 sh "$package/start.sh" -soundcloud-setup "$soundcloud_mode"
 break
done
section 'Проверка доступа к SoundCloud'
if sh "$package/start.sh" -check-soundcloud > "$saved/soundcloud-check.log" 2>&1; then
 ok 'Проверка SoundCloud завершена.'
else
 warn 'Не удалось проверить каталог и аудиопоток SoundCloud.'
 info 'Причина может быть в доступе к сервису, авторизации или настройках.'
 info 'Можно проверить SOCKS5-прокси, который будет использовать только SoundCloud.'
 info 'Формат: socks5://host:port или socks5://user:password@host:port. Ввод скрыт.'
 while :; do
  secret 'SOCKS5 URL (Enter — сохранить прежние настройки и продолжить):'
  soundcloud_proxy=$answer
  if [ -z "$soundcloud_proxy" ]; then
   warn 'Настройки прокси сохранены без изменений. SoundCloud может оставаться недоступным.'
   break
  fi
  if printf '%s\n' "$soundcloud_proxy" |
     sh "$package/start.sh" -check-soundcloud-proxy > "$saved/soundcloud-check.log" 2>&1; then
   save_soundcloud_proxy
   ok 'Доступ к SoundCloud через SOCKS5 проверен; настройки сохранены.'
   break
  fi
  warn 'Проверка через SOCKS5 не прошла. Проверьте URL, доступ и авторизацию или нажмите Enter.'
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
section 'Установка и запуск'
run_step 'Установка Quasar Bridge и настройка автозапуска' sh "$package/install.sh"
committed=true
if [ "$configure" = false ] && ! run_step 'Проверка нейросети и Home Assistant' sh "$state/app/control.sh" check-backends; then
 error 'Программа обновлена, но сервисы недоступны. Запустите установку повторно и измените настройки.'
 exit 1
fi
if [ -s "$state/endpoint-backup.json" ]; then
 summary
 ok 'Подключение Алисы сохранено, перезагрузка не требуется.'
 exit 0
fi
section 'Подключение Алисы'
ask 'Подключить Алису к Quasar Bridge? [Y/n]'
answer=${answer:-y}
if yes; then
 run_step 'Подключение Алисы к Quasar Bridge' sh "$state/app/control.sh" apply
 ask 'Для применения подключения нужна перезагрузка. Перезагрузить сейчас? [Y/n]'
 answer=${answer:-y}
 summary
 if yes; then
  info 'Перезагрузка колонки…'
  reboot
 else
  warn 'Перезагрузите колонку, чтобы Алиса начала работать через Quasar Bridge.'
 fi
else
 summary
 info 'Чтобы подключить Алису позже, выполните на колонке и затем перезагрузите её:'
 printf '  sh %s/app/control.sh apply\n' "$state"
fi
