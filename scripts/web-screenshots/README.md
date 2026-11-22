# Снимки веб-панели

Workflow **web panel screenshots** собирает Bridge и публикует артефакт
`web-panel-screenshots` с вкладками «Обзор» и «Чат». На стенде нет реальной
колонки, аккаунта или API-ключа: Bridge отвечает через отдельный синтетический
OpenAI-совместимый бэкенд, оба слушают только loopback.

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

Для обновления README скачайте артефакт и проверьте оба изображения перед
заменой файлов в `docs/images/`. Workflow не коммитит результаты.
