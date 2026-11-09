# Проверка Bridge в kuyase

Workflow `.github/workflows/kuyase.yml` запускается на push, pull request и
вручную через Actions → station emulator → Run workflow. Секреты не нужны;
используется GitHub hosted runner Ubuntu 24.04 и QEMU с программной эмуляцией ARM.
В runner устанавливается `ipxe-qemu`: его `efi-virtio.rom` нужен для сетевой карты
`virtio-net-pci`, даже при headless-загрузке напрямую из ядра.

Матрица: Mini 1 (`yandexmini`) и Станция 2 (`station2`), в каждой VM последовательно
проверяются standalone пакеты Bridge ARM64 и ARMv7 из текущего checkout.
Kuyase 1.1.0 собирается из закреплённого коммита на Ubuntu 24.04: готовый Linux
бинарник upstream требует `GLIBC_2.43` и на этом runner не запускается.
Отдельный job собирает host CLI один раз и передаёт его обеим VM. Исходники,
гостевое ядро/мост и обе публичные OTA проверяются по SHA-256. Скрипт `prepare.py` читает
контейнеры XZ/CPIO и UBIFS, сохраняет права inode и передаёт rootfs в `kuyase prepare`.
На Mini 1 штатный `adbd_backup` копируется как `adbd` для доступа в эмулятор.
Исходные установщики OTA не запускаются; VM работает в stub/snapshot режиме.

Проверяется настоящий исполняемый файл: `-check-config`, `/healthz`, WebSocket
`VoiceInput` → локальный mock ASR → русская команда включения/выключения AirPlay,
ответ Bridge, сохранение состояния после закрытия запроса и завершение процесса.
Перед запуском Bridge на настоящих файлах прошивки (`/system/vendor/quasar`) проверяется
второе слово вызова: `quasar-bridge -speaker-wake status`, затем для `yandex` и `alisa` включение,
сверка `customActivation` в `quasar.cfg` и содержимого `custom_activation` с моделью прошивки,
повторное включение с отказом и откат до байтового совпадения `quasar.cfg`.
Распознавание слов по звуку не проверяется: в эмуляторе нет микрофона.
Mock слушает только loopback; неожиданные обращения к облаку/LLM завершают тест
ошибкой. При любой ошибке job падает, а доступные логи сохраняются на 7 дней
в артефактах `kuyase-<board>-logs`.
`qemu-serial.log` содержит консоль ядра и stderr QEMU; при ошибке он также
выводится в лог задания, чтобы причина не скрывалась за `Connection refused`.

Это проверка запуска и управления в эмуляторе. Передача аудио, физический DAC,
iPhone, обнаружение mDNS в локальной сети и реальные облачные сервисы здесь
не проверяются. Для них нужны отдельные испытания.

Локальный запуск на Linux с `qemu-system-aarch64`, `adb`, `curl`, Python 3 и Go:

```sh
python3 -m venv runtime/kuyase-ci-venv
runtime/kuyase-ci-venv/bin/pip install ubi-reader==0.8.16
# Нужны также Rust/rustup и native build dependencies из workflow.
rustup toolchain install stable --profile minimal
bash scripts/kuyase/build-emulator.sh runtime/kuyase-ci
runtime/kuyase-ci-venv/bin/python scripts/kuyase/prepare.py \
  --board station2 --emulator-dir runtime/kuyase-ci --output runtime/kuyase-ci-input
python3 scripts/build-speaker-package.py --arch arm64
python3 scripts/build-speaker-package.py --arch armv7
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/kuyase-control ./scripts/kuyase/control
python3 scripts/kuyase/run.py --board station2 \
  --emulator-dir runtime/kuyase-ci --build-dir dist --output dist/kuyase-results/station2
```

Порты хоста `127.0.0.1:15555`, `18090`, `18091` должны быть свободны.
Другие запущенные VM или физические колонки скрипты не останавливают.
