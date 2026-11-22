# Снимки веб-панели

Workflow **web panel screenshots** собирает Bridge и публикует артефакт
`web-panel-screenshots` с вкладками «Обзор», «Маршрутизация» и «Чат». Сценарий проверяет редактирование
и приоритет правил, пробную фразу со словом активации, выбор доступного обработчика,
два способа возврата из ChatGPT и ширину панелей на мобильном экране.
Bridge и тестовый сервер модели работают на loopback. Браузер имитирует отказ
на странице согласия ChatGPT. Файл тестовой регистрации хранится во временной папке.

Запуск из корня репозитория (Node.js 22+, Go из `go.mod` и Chromium Playwright):

```sh
out="$(mktemp -d)"
CGO_ENABLED=0 go build -trimpath -o "$out/quasar-bridge" ./cmd/quasar-bridge
(
  cd scripts/web-screenshots
  npm ci --ignore-scripts --no-audit --no-fund
  ./node_modules/.bin/playwright install chromium
  node capture.mjs "$out/quasar-bridge" "$out/screenshots"
)
```

Для обновления README скачайте артефакт и проверьте изображения перед
заменой файлов в `docs/images/`. Workflow не коммитит результаты.
