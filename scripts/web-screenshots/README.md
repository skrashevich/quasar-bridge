# Снимки веб-панели

При открытии pull request и каждом новом коммите в нём workflow **web panel screenshots**
собирает Bridge, проверяет панель в Chromium и публикует снимки в артефакте
`web-panel-screenshots`. Проверка запускается при изменениях в любых файлах.

| Экран | Файлы PNG |
| --- | --- |
| Обзор колонки в светлой и тёмной темах | `web-panel-overview.png`, `web-panel-overview-dark.png` |
| Настройки Bridge | `web-panel-settings.png` |
| Проверка маршрута и пробная фраза внутри редактора правила | `web-panel-routing.png`, `web-panel-rule-editor.png` |
| Вход в ChatGPT по адресу колонки и выбор модели подписки | `web-panel-chatgpt-login.png`, `web-panel-chatgpt-model.png` |
| Чат с ответом на уточняющий вопрос | `web-panel-chat.png` |
| История на компьютере и телефоне | `web-panel-history.png`, `web-panel-history-mobile.png` |

Bridge и тестовый сервер модели работают на loopback. Playwright проверяет совпадение правила перед сохранением, ручной и автоматический возврат из ChatGPT, выбор модели, поиск по истории и мобильные фильтры. Каталог моделей и статус аккаунта задаёт сценарий. История читается из временного журнала с примерами диалогов.

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
