# Свой голос

Мост отправляет текст ответа в ваш сервис озвучки, а не в Яндекс, и передаёт полученный звук колонке.

## Что меняется

| Кто отвечает | Голос |
|---|---|
| Ваша модель и Home Assistant | Ваш |
| Яндекс, только речь (погода и подобное) | Ваш, если `revoice_yandex: true` |
| Яндекс, потоковые ответы и ответы с действием (таймер, музыка) | Алиса |
| Сервис озвучки недоступен или вернул не тот формат | Алиса: колонка не молчит |

Распознавание речи и слово активации «Алиса» остаются прежними. Для распознавания нужен аккаунт и облако Яндекса.

## Без компьютера: другой голос Яндекса

Самый простой вариант: синтез остаётся в облаке Яндекса, но с другим голосом. На колонке в `/data/opt/quasar-bridge/config.json`:

```json
"tts": {
  "enabled": true,
  "yandex_voice": "ermil",
  "revoice_yandex": true
}
```

Необязательно: `yandex_emotion` (`good`, `neutral`, `evil`) и `yandex_speed` (от 0.1 до 3). Примените: `sh /data/opt/quasar-bridge/app/control.sh restart`.

Какие голоса принимает сервер колонки, проверяется только опытом: попробуйте `ermil`, `zahar`, `jane`, `oksana`, `omazh`, `alena`, `filipp`. Если голос не поменялся, сервер игнорирует этот параметр или это имя.

## 1. Запустите сервис озвучки

На компьютере в одной сети с колонкой, который включён, пока вы пользуетесь колонкой. Нужны Python 3 и ffmpeg.

```sh
brew install ffmpeg                       # macOS
python3 -m venv ~/voice && ~/voice/bin/pip install piper-tts
mkdir -p ~/voice/models
~/voice/bin/python -m piper.download_voices --download-dir ~/voice/models ru_RU-denis-medium
python3 tools/voice-server/server.py --voices ~/voice/models --piper ~/voice/bin/piper
```

Другие русские голоса Piper: `ru_RU-irina-medium`, `ru_RU-ruslan-medium`, `ru_RU-dmitri-medium`. Голос можно обучить и самому: сервис подхватит любой файл `.onnx` из папки вместе с его `.onnx.json`.

Проверка с этого же компьютера:

```sh
curl -o test.ogg http://localhost:8880/v1/audio/speech \
  -d '{"input":"Привет, это мой новый голос.","voice":"ru_RU-denis-medium"}'
```

Любой другой движок подключается как команда: текст приходит на stdin, звук в формате, который понимает ffmpeg, уходит на stdout.

```sh
python3 tools/voice-server/server.py --command 'мой=/путь/к/команде --аргумент'
```

Подойдёт и готовый сервис с OpenAI-совместимым `/audio/speech`, если он возвращает моно Ogg/Opus при `response_format: "opus"`. Тогда `tools/voice-server` не нужен.

Сервис слушает всю локальную сеть. Чтобы ограничить доступ, запустите его с `--token ключ`, а ключ укажите в `QUASAR_TTS_API_KEY` в `/data/opt/quasar-bridge/runtime.env`. В интернет порт не открывайте.

## 2. Включите голос в мосте

В `/data/opt/quasar-bridge/config.json` на колонке добавьте блок. Вместо адреса подставьте IP компьютера, лучше закрепив его на роутере:

```json
"tts": {
  "enabled": true,
  "base_url": "http://voice.example.invalid:8880/v1",
  "voice": "ru_RU-denis-medium",
  "revoice_yandex": true
}
```

| Поле | Значение |
|---|---|
| `base_url` | Адрес сервиса с `/v1` |
| `voice` | Имя голоса: файл модели без `.onnx` |
| `model` | Нужно только внешним сервисам |
| `timeout_ms` | Сколько ждать начала ответа, потом отвечает Алиса. По умолчанию 15000 |
| `revoice_yandex` | Озвучивать своим голосом простые ответы Яндекса. По умолчанию выключено |

Примените и проверьте на колонке:

```sh
sh /data/opt/quasar-bridge/app/control.sh restart
sh /data/opt/quasar-bridge/app/control.sh check-backends   # ожидается «Voice: ok»
```

Затем скажите: «Алиса, спроси нейросеть: почему небо голубое?».

Сменить голос: измените `voice` и перезапустите мост. Отключить: `"enabled": false`, вернётся голос Алисы.

## Если что-то не так

В `sh /data/opt/quasar-bridge/app/control.sh logs` ищите:

| Запись | Значение |
|---|---|
| `custom_voice_failed` | Сервис не ответил или вернул не тот формат. Подробность в поле `error` |
| `native_revoice` | Ответ Яндекса переозвучен |
| `native_tts_ready` | Звук передан колонке |

С `revoice_yandex` простой ответ Яндекса звучит с небольшой задержкой: сначала приходит текст, затем работает синтез.
