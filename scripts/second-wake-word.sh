#!/bin/sh
# Второе слово вызова на колонке: рядом со словом, выбранным в приложении
# («Алиса» или «Яндекс»), включается второе слово из готовых моделей прошивки.
#
# Прошивка умеет держать два компонента слова: обычный (его выбирают в приложении)
# и «custom_activation». Скрипт кладёт в папку второго слота копию модели другого
# слова и включает слот в quasar.cfg. После перезагрузки колонка откликается на оба
# слова, а мост видит, каким из них разбудили (routing.wake_routes).
#
# Проверено: Яндекс Станция Миди (плата Yandex Cucumber DVT2), quasar 3.21,
# Linux 5.4.180. На других моделях и прошивках не проверялось.
#
# Использование (от root, на колонке):
#   sh second-wake-word.sh enable [alisa|yandex]   без аргумента: слово, которое
#                                                  не выбрано в приложении
#   sh second-wake-word.sh disable                 вернуть исходное состояние
#   sh second-wake-word.sh status
# После enable и disable колонку нужно перезагрузить (reboot).
#
# Что меняется: в quasar.cfg вставляется блок "customActivation" в раздел "aliced"
# и создаётся /system/vendor/quasar/spotter_models/custom_activation (с меткой
# .second-wake-word). Корень на время записи монтируется на запись и возвращается
# в режим «только чтение». Исходный quasar.cfg сохраняется в
# $QUASAR_DATA_DIR/second-wake-word/quasar.cfg.orig. После обновления прошивки
# запустите enable ещё раз.
set -eu
umask 022
cfg=${QUASAR_CFG:-/system/vendor/quasar/quasar.cfg}
models=${QUASAR_SPOTTER_MODELS:-/system/vendor/quasar/spotter_models}
user_config=${QUASAR_USER_CONFIG:-/data/quasar/data/user_config.dat}
state=${QUASAR_DATA_DIR:-/data/opt/quasar-bridge}/second-wake-word
slot=$models/custom_activation
marker=.second-wake-word
remounted=false

fail() { echo "$*" >&2; exit 1; }

# Те же действия, что в firmware.sh установщика: корень по умолчанию только для чтения.
make_writable() {
  test "${QUASAR_NO_REMOUNT:-}" = 1 && return 0
  if awk '$2 == "/" && $4 ~ /(^|,)ro(,|$)/ {found=1} END {exit !found}' /proc/mounts 2>/dev/null; then
    mount -o remount,rw /
    remounted=true
  fi
}
restore_mode() {
  sync
  if [ "$remounted" = true ]; then
    mount -o remount,ro / || echo 'Не удалось вернуть корень в режим «только чтение»; перезагрузите колонку.' >&2
    remounted=false
  fi
}

need_root() {
  test "${QUASAR_NO_REMOUNT:-}" = 1 && return 0
  test "$(id -u)" = 0 || fail 'Запустите от root.'
}

# Слово, выбранное в приложении, хранится в настройках аккаунта: "spotter":"alisa".
selected_word() {
  grep -o '"spotter":"[a-z]*"' "$user_config" 2>/dev/null | head -n 1 | sed 's/.*:"\(.*\)"/\1/' || true
}

has_block() { grep -q '"customActivation":{' "$cfg"; }

enable() {
  need_root
  word=${1:-}
  if [ -z "$word" ]; then
    case "$(selected_word)" in
      alisa) word=yandex ;;
      yandex) word=alisa ;;
      *) fail 'Не удалось определить слово из приложения. Укажите второе слово: enable alisa или enable yandex.' ;;
    esac
  fi
  case "$word" in alisa | yandex) ;; *) fail 'Слово должно быть alisa или yandex.' ;; esac
  test -f "$models/activation/$word/acoustic_model.nnet" || fail "В прошивке нет модели слова $word ($models/activation/$word)."
  test -f "$cfg" || fail "Нет файла $cfg."
  has_block && fail 'Второе слово уже включено. Сначала выполните disable.'
  # Блок вставляется сразу после "aliced":{ — это место должно быть ровно одно.
  test "$(grep -o '"aliced":{' "$cfg" | wc -l)" -eq 1 || fail 'В quasar.cfg не найден единственный раздел "aliced": прошивка устроена иначе, изменения не вносились.'
  if [ -e "$slot" ] && [ ! -e "$slot/$marker" ]; then
    fail "$slot уже существует и создан не этим скриптом. Изменения не вносились."
  fi
  mkdir -p "$state"
  test -e "$state/quasar.cfg.orig" || cp -p "$cfg" "$state/quasar.cfg.orig"
  make_writable
  trap restore_mode EXIT
  rm -rf "$slot.new"
  cp -R "$models/activation/$word" "$slot.new"
  : > "$slot.new/$marker"
  rm -rf "$slot"
  mv "$slot.new" "$slot"
  sed "s/\"aliced\":{/&\"customActivation\":{\"forceEnable\":true,\"forcePhrase\":\"$word\"},/" "$cfg" > "$cfg.tmp"
  if ! grep -q '"customActivation":{' "$cfg.tmp"; then
    rm -f "$cfg.tmp"
    fail 'Не удалось подготовить новый quasar.cfg; изменения отменены.'
  fi
  chmod 0644 "$cfg.tmp"
  mv "$cfg.tmp" "$cfg"
  echo "Второе слово ($word) включено. Перезагрузите колонку (reboot). Откат: sh $0 disable"
}

disable() {
  need_root
  make_writable
  trap restore_mode EXIT
  if has_block; then
    sed 's/"customActivation":{"forceEnable":true,"forcePhrase":"[a-z]*"},//' "$cfg" > "$cfg.tmp"
    if grep -q '"customActivation":{' "$cfg.tmp"; then
      rm -f "$cfg.tmp"
      fail 'Блок customActivation в quasar.cfg отличается от записанного скриптом; уберите его вручную (исходный файл: '"$state"'/quasar.cfg.orig).'
    fi
    chmod 0644 "$cfg.tmp"
    mv "$cfg.tmp" "$cfg"
  fi
  if [ -e "$slot/$marker" ]; then
    rm -rf "$slot"
  elif [ -e "$slot" ]; then
    echo "$slot создан не этим скриптом, оставлен как есть." >&2
  fi
  echo 'Второе слово выключено. Перезагрузите колонку (reboot).'
}

status() {
  if has_block; then echo "quasar.cfg: блок customActivation есть"; else echo "quasar.cfg: блока customActivation нет"; fi
  if [ -e "$slot/$marker" ]; then echo "модель: $slot создана скриптом"; elif [ -e "$slot" ]; then echo "модель: $slot есть, но создана не скриптом"; else echo "модель: $slot нет"; fi
  echo "слово в приложении: $(selected_word)"
}

case "${1:-}" in
  enable) shift; enable "$@" ;;
  disable) disable ;;
  status) status ;;
  *) fail "Использование: sh $0 enable [alisa|yandex] | disable | status" ;;
esac
