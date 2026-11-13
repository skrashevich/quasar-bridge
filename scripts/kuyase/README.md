# E2E-проверка Bridge в КУЯСЭ

Workflow `.github/workflows/kuyase.yml` запускается на push, pull request,
вручную через Actions → station emulator → Run workflow и через `workflow_call`.
Секреты не нужны: все внешние сервисы заменены локальными HTTP/WebSocket
фикстурами внутри гостевой системы. Проверяется настоящий standalone-пакет
Bridge, собранный из текущего checkout, в настоящей прошивке станции.

## Матрица

| Прошивка и профиль КУЯСЭ | Пакеты Bridge |
| --- | --- |
| Mini 1 (`yandexmini`) | ARM64, ARMv7 |
| Станция 2 (`station2`) | ARM64, ARMv7 |

Каждое сочетание — отдельный job с отдельной VM. `fail-fast: false` позволяет
получить результаты остальных сочетаний после обнаружения регрессии.
Проверяются две разные прошивки, а не только разные аппаратные профили на
одном rootfs. Другие модели требуют отдельного проверенного образа прошивки;
их совместимость эти проверки не подтверждают.

## Сценарии в каждой VM

| Сценарий | Проверяемое поведение |
| --- | --- |
| `config` | Standalone-бинарник принимает конфигурацию (`-check-config`) |
| `health` | Работает `/healthz`, включён hybrid и доступен AirPlay |
| `weather`, `music`, `timer` | VoiceInput → ASR → разрешение облачного запроса → ответ Яндекса; LLM и HA не вызываются |
| `llm` | VoiceInput → ASR → HTTP Chat Completions → ответ Bridge и запрос синтеза TTS |
| `text` | TextInput → LLM → ответ и TTS без ASR |
| `dialogue` | После закрытия WebSocket и переподключения LLM получает предыдущие вопрос и ответ |
| `isolation` | Другая колонка не получает чужой контекст |
| `reset` | Сброс диалога не вызывает LLM, следующий вопрос приходит без старой истории |
| `home` | MCP inventory/exposure → план LLM → ровно один service call → подтверждение состояния; вопрос о состоянии не выполняет действий |
| `failure` | HTTP 503 от LLM даёт понятный ответ; следующий запрос того же устройства проходит без ошибочного контекста |
| `start`, `stop` | Голосовое включение/выключение настоящего AirPlay-приёмника; состояние сохраняется после закрытия WebSocket |
| `airplay-cleanup` | Приёмник остановлен, процессов `station-pcm` не осталось |
| `shutdown` | Процесс Bridge завершился и PID больше не существует |

Для каждого запроса контроллер проверяет точный ответ, связь ответа с исходным
request ID, неизменность ASR и число обращений к каждому бэкенду. Локальный
обработчик Яндекса возвращает ответ только после `ContinueRequest`: обход
ожидания разрешения или отправка локальной команды в облако приводят к ошибке.
Фикстура LLM проверяет сообщения истории, а MCP хранит изменяемое состояние
света и проверяет цель команды. Неожиданные HTTP/MCP/протокольные запросы
завершают проверку ошибкой. Классификатор выключен для воспроизводимости:
проверяются штатные правила и контекст диалога.

Ошибка сценария не мешает остальным сценариям той же VM; ошибка запуска или
инфраструктуры завершает job. В обоих случаях workflow возвращает ошибку.
После завершения доступны:

- таблица сценариев и длительности в GitHub Actions Summary;
- `result.json` и `junit.xml` с результатами отдельных сценариев;
- логи Bridge, локальных бэкендов, гостевой системы, VM и QEMU;
- при сбое подготовки — `prepare.log`.

Артефакты `kuyase-<board>-<arch>-logs` хранятся 7 дней. Длительности служат
диагностикой; автоматический поиск регрессий здесь основан на проверках
поведения, без сравнения скорости разных shared runners.

## Эмулятор и прошивки

Используется Ubuntu 24.04 и QEMU с программной эмуляцией ARM.
`ipxe-qemu` предоставляет `efi-virtio.rom` для `virtio-net-pci`.
КУЯСЭ 1.4.0 собирается из закреплённого коммита на runner: готовый upstream
Linux CLI требует более новую glibc. CLI собирается один раз для всей матрицы.
Исходники, гостевое ядро/мост и обе публичные OTA проверяются по SHA-256.
`prepare.py` читает XZ/CPIO и UBIFS, сохраняет права inode и передаёт rootfs
в `kuyase prepare`. На Mini 1 штатный `adbd_backup` копируется как `adbd`.
Исходные установщики OTA не запускаются; VM работает в stub/snapshot режиме.

Распознавание и облачные ответы здесь синтетические. Проверяется запрос TTS,
но аудиопоток не синтезируется. Передача аудио, физический DAC, iPhone,
обнаружение mDNS в локальной сети, реальные облачные сервисы и Home Assistant
требуют отдельных испытаний. Установщик и обновление пакетов этим набором
также не проверяются.

## Локальный запуск

Нужны Linux, `qemu-system-aarch64`, `adb`, `curl`, Python 3 и Go, а также
Rust/rustup и native build dependencies из workflow.

```sh
python3 -m venv runtime/kuyase-ci-venv
runtime/kuyase-ci-venv/bin/pip install ubi-reader==0.8.16
rustup toolchain install stable --profile minimal
bash scripts/kuyase/build-emulator.sh runtime/kuyase-ci
runtime/kuyase-ci-venv/bin/python scripts/kuyase/prepare.py \
  --board station2 --emulator-dir runtime/kuyase-ci --output runtime/kuyase-ci-input
python3 scripts/build-speaker-package.py --arch arm64
python3 scripts/build-speaker-package.py --arch armv7
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/kuyase-control ./scripts/kuyase/control
python3 scripts/kuyase/run.py --board station2 --arch arm64 \
  --emulator-dir runtime/kuyase-ci --build-dir dist --output dist/kuyase-results/station2-arm64
```

Без `--arch` обе сборки проверяются последовательно с новым экземпляром
фикстур для каждой сборки. Порты хоста `127.0.0.1:15555`, `18090`, `18091`
должны быть свободны. Скрипт завершает только созданные им процессы VM;
существующие VM и физические колонки не останавливает.

Контроллер и отчётность можно проверить без VM:

```sh
go test -race ./scripts/kuyase/control -count=1 -timeout=60s
python3 -m unittest discover -s scripts/kuyase -p 'test_*.py'
```

Go-тест контроллера использует Proxy/Native/Engine и HTTP-клиенты LLM/MCP,
но не standalone-процесс и не настоящий приёмник. E2E-совместимость прошивки
подтверждает только отдельный прогон VM.
