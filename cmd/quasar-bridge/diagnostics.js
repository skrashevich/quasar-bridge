"use strict";
const log = document.getElementById("log");
const connection = document.getElementById("connection");
const pause = document.getElementById("pause");
let cursor = 0;
let run = "";
let text = "";
let paused = false;
function readableLogs(raw) {
  return raw.split("\n").map(line => {
    if (!line) return "";
    try {
      const { time, level, msg, ...fields } = JSON.parse(line);
      const details = Object.entries(fields).map(([key, value]) => key + "=" + JSON.stringify(value));
      return [new Date(time).toLocaleTimeString("ru-RU"), level, msg, ...details].join(" ");
    } catch (_) {
      return line;
    }
  }).join("\n");
}
pause.addEventListener("click", () => {
  paused = !paused;
  pause.textContent = paused ? "Продолжить обновление" : "Приостановить обновление";
  pause.setAttribute("aria-pressed", String(paused));
  if (paused) connection.textContent = "Обновление страницы приостановлено. Bridge продолжает записывать события.";
});
async function refresh() {
  if (!paused && !document.hidden) {
    try {
      const query = new URLSearchParams({ after: String(cursor), run });
      const response = await fetch("logs?" + query, { cache: "no-store", signal: AbortSignal.timeout(5000) });
      if (!response.ok) throw new Error(String(response.status));
      const data = await response.json();
      if (paused) {
        window.setTimeout(refresh, 1500);
        return;
      }
      const atEnd = log.scrollHeight - log.scrollTop - log.clientHeight < 48;
      if (data.reset || run !== data.run) text = "";
      run = data.run;
      cursor = data.cursor;
      text += data.text;
      if (text.length > 1048576) {
        text = text.slice(-1048576);
        text = text.slice(text.indexOf("\n") + 1);
      }
      log.textContent = text ? readableLogs(text) : "Событий пока нет. Включите AirPlay и подключите телефон.";
      if (atEnd) log.scrollTop = log.scrollHeight;
      const state = data.airplay || {};
      const states = { running: "Включён", stopped: "Выключен", starting: "Запускается", stopping: "Выключается" };
      document.getElementById("receiver").textContent = state.enabled === false ? "Недоступен" : (states[state.state] || "Неизвестно");
      document.getElementById("sender").textContent = state.connected ? "Подключён" : "Не подключён";
      document.getElementById("playing").textContent = state.playing ? "Запущено" : "Не запущено";
      document.getElementById("version").textContent = data.version;
      document.getElementById("limit").textContent = data.truncated
        ? "Начало журнала уже вышло за лимит 1 МиБ. Скачайте файл сейчас; полный сервисный журнал остаётся на колонке."
        : "Доступны события с запуска Bridge, до 1 МиБ. Файл содержит журнал, версию и текущее состояние AirPlay.";
      connection.className = "";
      connection.textContent = "Обновлено в " + new Date().toLocaleTimeString("ru-RU") + ". Новые события появляются автоматически.";
    } catch (_) {
      if (!paused) {
        connection.className = "error";
        connection.textContent = "Нет связи с Bridge. Проверьте SSH-подключение; страница попробует снова.";
      }
    }
  }
  window.setTimeout(refresh, 1500);
}
refresh();
