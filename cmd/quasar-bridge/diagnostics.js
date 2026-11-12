"use strict";

const byId = id => document.getElementById(id);
const pageNames = { overview: "Обзор", settings: "Настройки", query: "Проверка запроса", airplay: "AirPlay", history: "История", logs: "Журнал событий" };
const state = { page: "overview", status: null, config: null, restartRun: "", settings: new Map(), drafts: new Map(), configBusy: false, statusBusy: false, historyBusy: false, airplayBusy: false, queryBusy: false };
const logState = { cursor: 0, run: "", text: "", paused: false, busy: false };
const settingLabels = {
  mode: "Режим Bridge", listen: "Адрес HTTP-сервера", upstream: "Сервер Алисы", allowed_ips: "Разрешённые адреса",
  llm_base_url: "Адрес сервера модели", llm_model: "Модель", llm_api: "Формат API", llm_reasoning_effort: "Усилие рассуждения", llm_max_output_tokens: "Максимум токенов ответа", llm_hosted_tools: "Серверные инструменты", llm_chat_parameters: "Параметры генерации", llm_strip_prefixes: "Обращения к модели", llm_tts_markup: "Голоса и звуковые эффекты", home_warm_area: "Область Home Assistant", listen_after_reply: "Слушать после ответа",
  "dialogue_context.enabled": "Контекст диалога", "dialogue_context.ttl_seconds": "Время жизни контекста, с", "dialogue_context.max_turns": "Пар вопрос / ответ", "dialogue_context.max_chars": "Максимум символов контекста",
  "local_progress.enabled": "Звук ожидания ответа", "local_progress.after_ms": "Задержка первого звука, мс", "local_progress.interval_ms": "Интервал повторения, мс",
  "conversation_log.enabled": "Запись диалогов", "conversation_log.max_size_mb": "Размер журнала, МиБ", "conversation_log.max_backups": "Число архивов", "conversation_log.max_text_chars": "Символов в записи",
  "routing.default_route": "Маршрут по умолчанию", "routing.rules": "Правила маршрутизации", "routing.classifier.enabled": "Классификация моделью", "routing.classifier.prompt": "Инструкция классификатора", "routing.classifier.timeout_ms": "Тайм-аут классификации, мс", "routing.classifier.on_error": "Маршрут при ошибке", "routing.refusal.enabled": "Передавать модели после отказа Алисы", "routing.refusal.prefixes": "Начала фраз отказа", "routing.refusal.contains_any": "Фразы отказа",
  "assistant_tools.enabled": "Инструменты модели", "assistant_tools.web": "Чтение веб-страниц", "assistant_tools.history": "Поиск в истории", "assistant_tools.time": "Текущее время", "assistant_tools.updates": "Обновления Bridge", "assistant_tools.play_sound": "Воспроизведение звука", "assistant_tools.configuration": "Управление настройками голосом", "assistant_tools.timezone": "Часовой пояс", "assistant_tools.max_rounds": "Раундов инструментов", "assistant_tools.max_calls": "Вызовов за раунд", "assistant_tools.web_timeout_ms": "Тайм-аут веб-запроса, мс", "assistant_tools.web_max_bytes": "Размер веб-ответа, байт", "assistant_tools.web_max_chars": "Символов текста страницы", "assistant_tools.history_max_scan_mb": "Объём поиска в истории, МиБ", "assistant_tools.mcp_servers": "MCP-серверы",
  "airplay.disabled": "Полностью отключить AirPlay", "airplay.name": "Имя приёмника", "airplay.interface": "Сетевой интерфейс", "airplay.listen_address": "Адрес приёмника", "airplay.device": "Аудиоустройство ALSA", "airplay.output_delay_ms": "Задержка аудио, мс", "airplay.initial_volume_db": "Начальная громкость, дБ",
  "diagnostics.network_access": "Открыть панель в локальной сети"
};
const enumSettings = {
  mode: [["hybrid", "Гибридный"], ["observe", "Наблюдение"]],
  llm_api: [["chat_completions", "Chat Completions"], ["responses", "Responses"], ["chatgpt", "ChatGPT"]],
  llm_reasoning_effort: [["", "По умолчанию"], ["none", "Без рассуждения"], ["minimal", "Минимальное"], ["low", "Низкое"], ["medium", "Среднее"], ["high", "Высокое"], ["xhigh", "Очень высокое"], ["max", "Максимальное"]],
  "routing.default_route": [["classify", "Классификатор"], ["yandex", "Алиса"], ["llm", "Модель"], ["home", "Умный дом"]],
  "routing.classifier.on_error": [["yandex", "Алиса"], ["llm", "Модель"], ["home", "Умный дом"]]
};
const sourceNames = { rule: "Совпало правило", default: "Маршрут по умолчанию", classifier: "Классификатор", airplay: "Команда AirPlay", mode: "Режим Bridge", pending: "Нужен классификатор", builtin: "Встроенная команда", bridge_configuration: "Команда управления Bridge" };
function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function message(id, text = "", error = false) {
  const node = byId(id);
  node.textContent = text;
  node.classList.toggle("error", error);
  node.hidden = !text;
}
function routeName(route) {
  return ({ yandex: "Алиса", llm: "Модель", home: "Умный дом", airplay: "AirPlay", classify: "Классификатор", proxy: "Прокси", observe: "Наблюдение" })[route] || route || "Не определён";
}
function timeText(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value || "") : date.toLocaleString("ru-RU");
}
function uptimeText(seconds) {
  if (typeof seconds !== "number") return "—";
  const days = Math.floor(seconds / 86400), hours = Math.floor(seconds % 86400 / 3600), minutes = Math.floor(seconds % 3600 / 60);
  return [days ? days + " д" : "", hours ? hours + " ч" : "", minutes ? minutes + " мин" : "", !days && !hours && !minutes ? Math.floor(seconds) + " с" : ""].filter(Boolean).join(" ");
}
async function request(path, options = {}, timeout = 15000) {
  const controller = new AbortController();
  const timer = window.setTimeout(() => controller.abort(), timeout);
  try {
    const response = await fetch(path, { cache: "no-store", ...options, signal: controller.signal });
    let data;
    try { data = await response.json(); } catch (_) { throw new Error("Bridge вернул некорректный ответ (HTTP " + response.status + ")."); }
    if (!response.ok) throw new Error(data.error || "Ошибка HTTP " + response.status);
    return data;
  } catch (error) {
    if (error.name === "AbortError") throw new Error("Bridge не ответил вовремя. Проверьте соединение и попробуйте снова.");
    if (error instanceof TypeError) throw new Error("Нет связи с Bridge. Проверьте адрес панели и SSH-туннель, если он используется.");
    throw error;
  } finally { window.clearTimeout(timer); }
}
const post = (path, data, timeout) => request(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }, timeout);
function fillFacts(id, entries) {
  byId(id).replaceChildren(...entries.map(([name, value]) => {
    const row = element("div"); row.append(element("dt", "", name), element("dd", "", String(value))); return row;
  }));
}
function setConnected(connected) {
  byId("connection-state").classList.toggle("offline", !connected);
  byId("connection-text").textContent = connected ? "Bridge на связи" : "Нет связи";
}
function renderAirPlay(airplay = {}) {
  const names = { running: "Включён", stopped: "Выключен", starting: "Запускается", stopping: "Выключается" };
  byId("receiver").textContent = airplay.enabled === false ? "Отключён в настройках" : (names[airplay.state] || "Неизвестно");
  byId("sender").textContent = airplay.connected ? "Подключён" : "Не подключён";
  byId("playing").textContent = airplay.playing ? "Запущено" : "Не запущено";
  byId("airplay-name").textContent = airplay.name || "Приёмник AirPlay";
  byId("airplay-volume").textContent = typeof airplay.volume_db === "number" ? "Громкость: " + airplay.volume_db.toFixed(1) + " дБ" : "";
  byId("airplay-error").textContent = airplay.error || "";
  byId("airplay-error").hidden = !airplay.error;
  const available = state.status?.capabilities?.airplay !== false && airplay.enabled !== false;
  document.querySelectorAll("[data-airplay]").forEach(button => {
    const action = button.dataset.airplay;
    button.disabled = state.airplayBusy || !available || (action === "start" ? airplay.state === "running" || airplay.state === "starting" : action === "stop" ? airplay.state === "stopped" || airplay.state === "stopping" : !airplay.connected);
  });
  byId("set-volume").disabled = state.airplayBusy || !available || !airplay.connected;
  byId("volume-level").disabled = state.airplayBusy || !available || !airplay.connected;
}
function renderStatus(data) {
  state.status = data;
  setConnected(true);
  if (data.version) byId("version").textContent = "Версия " + data.version;
  byId("overview-mode").textContent = ({ hybrid: "Гибридный режим", observe: "Наблюдение", proxy: "Прокси" })[data.mode] || data.mode || "Работает";
  fillFacts("overview-state", [["Bridge", data.status === "ok" ? "Работает" : data.status || "Работает"], ["AirPlay", data.airplay?.enabled === false ? "Отключён" : ({ running: "Включён", stopped: "Выключен", starting: "Запускается", stopping: "Выключается" })[data.airplay?.state] || "Неизвестно"], ["История диалогов", data.conversation_log_enabled ? "Записывается" : "Запись отключена"]]);
  fillFacts("routing-state", [["По умолчанию", routeName(data.routing_default_route)], ["Классификация моделью", data.routing_classifier_enabled ? "Включена" : "Выключена"], ["Действующих правил", data.routing_rules ?? "—"]]);
  const features = [["Инструменты модели", data.assistant_tools_enabled], ["Контекст диалога", data.dialogue_context_enabled], ["Home Assistant", data.home_assistant_enabled], ["Воспроизведение музыки", data.music_playback_enabled], ["MCP-серверы", Number(data.mcp_servers_configured) > 0]];
  byId("feature-state").replaceChildren(...features.map(([name, on]) => element("span", "feature" + (on ? "" : " disabled"), name + (on ? ": включено" : ": отключено"))));
  fillFacts("technical-state", [["Время работы", uptimeText(data.uptime_seconds)], ["Кадров от колонки", data.client_frames ?? "—"], ["Кадров от Яндекса", data.server_frames ?? "—"], ["Ошибок записи истории", data.conversation_log_failures ?? "—"], ["Ревизия маршрутов", data.routing_revision || "—"], ["Панель по сети", data.diagnostics?.network_access ? "Доступна" : "Только localhost"], ["Адрес панели", data.diagnostics?.address || "—"], ["Порт панели", data.diagnostics?.port ?? "—"]]);
  byId("status-json").textContent = JSON.stringify(data, null, 2);
  renderAirPlay(data.airplay);
  byId("execute-query").disabled = state.queryBusy || data.capabilities?.query === false;
  if (data.capabilities?.query === false) byId("execute-query").title = "Выполнение запроса доступно в гибридном режиме";
  else byId("execute-query").removeAttribute("title");
  byId("execute-help").textContent = data.capabilities?.query === false ? "Выполнение запроса доступно в гибридном режиме. Режим можно изменить в настройках. Проверка правил выше доступна и в режиме наблюдения." : "Bridge действительно обработает фразу. Запрос может управлять устройствами, включать звук или менять настройки.";
  byId("last-updated").textContent = "Состояние обновлено в " + new Date().toLocaleTimeString("ru-RU");
}
async function loadStatus(reportError = false) {
  if (state.statusBusy) return;
  state.statusBusy = true;
  try {
    renderStatus(await request("api/status"));
    if (state.config?.restart_scheduled) await loadConfig(false);
    if (reportError) message("global-message");
  } catch (error) { setConnected(false); if (reportError) message("global-message", error.message, true); }
  finally { state.statusBusy = false; }
}
function settingGroup(path) {
  if (path.startsWith("diagnostics.") || ["mode", "listen", "upstream", "allowed_ips"].includes(path)) return "Bridge и доступ к панели";
  if (path.startsWith("llm_")) return "Модель и ответы";
  if (path.startsWith("routing.")) return "Маршрутизация";
  if (path.startsWith("dialogue_context.") || path.startsWith("local_progress.") || path === "listen_after_reply") return "Диалог и ожидание";
  if (path.startsWith("assistant_tools.")) return "Инструменты модели";
  if (path.startsWith("conversation_log.")) return "История диалогов";
  if (path.startsWith("airplay.")) return "AirPlay";
  if (path.startsWith("home_")) return "Умный дом";
  return "Дополнительные настройки";
}
function kindFor(setting) {
  if (enumSettings[setting.path]) return "select";
  if (typeof setting.effective === "boolean" || setting.description?.startsWith("Логическое")) return "boolean";
  if (typeof setting.effective === "number" || ["airplay.output_delay_ms", "airplay.initial_volume_db"].includes(setting.path)) return "number";
  if (setting.effective !== null && typeof setting.effective === "object" || /^(Массив|Объект)/.test(setting.description || "")) return "json";
  return "text";
}
function editableValue(setting) {
  const value = setting.effective;
  if (kindFor(setting) === "json") return JSON.stringify(value, null, 2);
  if (kindFor(setting) === "boolean") return value ? "true" : "false";
  return value === null || value === undefined ? "" : String(value);
}
function parseValue(setting, raw) {
  const kind = kindFor(setting);
  if (kind === "boolean") return raw === "true";
  if (kind === "json") {
    try { return JSON.parse(raw); } catch (_) { throw new Error("Введите корректный JSON: строки в двойных кавычках, без запятой после последнего элемента."); }
  }
  if (kind === "number") {
    if (!raw.trim()) {
      if (["airplay.output_delay_ms", "airplay.initial_volume_db"].includes(setting.path)) return null;
      throw new Error("Введите число.");
    }
    const value = Number(raw);
    if (!Number.isFinite(value)) throw new Error("Введите конечное число.");
    return value;
  }
  return raw;
}
function isDirty(setting) {
  if (!state.drafts.has(setting.path)) return false;
  try { return JSON.stringify(parseValue(setting, state.drafts.get(setting.path))) !== JSON.stringify(setting.effective); }
  catch (_) { return true; }
}
function dirtySettings() { return [...state.settings.values()].filter(isDirty); }
function shortValue(value) {
  const text = value === undefined ? "—" : JSON.stringify(value);
  return text.length > 240 ? text.slice(0, 240) + "…" : text;
}
function settingRow(setting, index) {
  const path = setting.path;
  const kind = kindFor(setting);
  const row = element("div", "setting-row" + (isDirty(setting) ? " dirty" : ""));
  row.dataset.settingPath = path;
  row.dataset.search = [path, setting.description, settingLabels[path] || ""].join(" ").toLocaleLowerCase("ru-RU");
  const description = element("div");
  const label = element("label", "", settingLabels[path] || path);
  label.htmlFor = "setting-" + index;
  const help = element("p", "setting-description", setting.description || ""); help.id = "setting-help-" + index;
  description.append(label, help, element("code", "setting-path", path));
  const flags = element("div", "setting-flags");
  flags.append(element("span", "badge", setting.restart_required ? "Требует перезапуска" : "Применяется без перезапуска"));
  if (setting.pending) flags.append(element("span", "badge pending", "Сохранено, ещё не действует"));
  if (setting.writable === false) flags.append(element("span", "badge", setting.environment ? "Задано окружением: " + setting.environment : "Только чтение"));
  description.append(flags);
  const editor = element("div", "setting-editor");
  let control;
  const raw = state.drafts.has(path) ? state.drafts.get(path) : editableValue(setting);
  if (kind === "select") {
    control = element("select");
    const options = [...enumSettings[path]];
    if (!options.some(([value]) => value === raw)) options.push([raw, raw]);
    for (const [value, text] of options) { const option = element("option", "", text); option.value = value; control.append(option); }
    control.value = raw;
  } else if (kind === "boolean") {
    control = element("input"); control.type = "checkbox"; control.checked = raw === "true";
    const switchLabel = element("label", "switch-label"); switchLabel.htmlFor = "setting-" + index;
    const caption = element("span", "", control.checked ? "Включено" : "Выключено");
    switchLabel.append(control, caption); editor.append(switchLabel);
  } else if (kind === "json" || path.endsWith(".prompt") || raw.length > 160) {
    control = element("textarea", path === "routing.rules" || path.endsWith(".prompt") ? "large-editor" : "");
    control.value = raw; control.rows = path === "routing.rules" ? 10 : 4; control.spellcheck = false;
  } else {
    control = element("input"); control.type = kind === "number" ? "number" : "text"; control.value = raw;
    if (kind === "number") { control.step = path === "airplay.initial_volume_db" ? "any" : "1"; control.placeholder = "По умолчанию"; }
  }
  control.id = "setting-" + index;
  control.disabled = setting.writable === false || state.configBusy;
  control.setAttribute("aria-describedby", help.id + " setting-error-" + index);
  control.dataset.path = path;
  if (kind !== "boolean") editor.append(control);
  const error = element("p", "field-error"); error.id = "setting-error-" + index; error.hidden = true; editor.append(error);
  control.addEventListener("input", () => {
    const rawValue = kind === "boolean" ? String(control.checked) : control.value;
    state.drafts.set(path, rawValue);
    if (kind === "boolean") control.nextElementSibling.textContent = control.checked ? "Включено" : "Выключено";
    row.classList.toggle("dirty", isDirty(setting));
    try { parseValue(setting, rawValue); error.hidden = true; control.removeAttribute("aria-invalid"); }
    catch (problem) { error.textContent = problem.message; error.hidden = false; control.setAttribute("aria-invalid", "true"); }
    updateConfigSummary();
  });
  if (setting.pending || setting.writable === false) {
    const values = element("div", "setting-values");
    for (const [name, value] of [["Действует: ", setting.active], ["В файле: ", setting.saved], ...(setting.writable === false ? [["С учётом окружения: ", setting.effective]] : [])]) {
      const line = element("div", "", name); line.append(element("code", "", shortValue(value))); values.append(line);
    }
    editor.append(values);
  }
  if (path === "diagnostics.network_access") editor.append(element("p", "network-notice", "Без авторизации: доступ получит любой клиент вашей локальной сети. Если отключить сетевой доступ при удалённом подключении, панель потеряет связь после применения. Для входа снова используйте SSH-туннель."));
  row.append(description, editor);
  return row;
}
function renderSettings() {
  const opened = new Map([...document.querySelectorAll(".settings-group")].map(group => [group.dataset.group, group.open]));
  const groups = new Map();
  let index = 0;
  for (const setting of state.settings.values()) {
    const group = settingGroup(setting.path);
    if (!groups.has(group)) groups.set(group, []);
    groups.get(group).push(settingRow(setting, index++));
  }
  const order = ["Bridge и доступ к панели", "Модель и ответы", "Маршрутизация", "Диалог и ожидание", "Инструменты модели", "История диалогов", "AirPlay", "Умный дом", "Дополнительные настройки"];
  byId("settings-fields").replaceChildren(...order.filter(name => groups.has(name)).map(name => {
    const section = element("details", "settings-group"); section.dataset.group = name;
    section.open = opened.has(name) ? opened.get(name) : ["Bridge и доступ к панели", "Модель и ответы"].includes(name);
    const summary = element("summary", "", name); summary.append(element("span", "group-count", String(groups.get(name).length)));
    section.append(summary, ...groups.get(name)); return section;
  }));
  filterSettings(); updateConfigSummary();
}
function filterSettings() {
  const query = byId("settings-search").value.trim().toLocaleLowerCase("ru-RU");
  let visible = 0;
  document.querySelectorAll(".settings-group").forEach(group => {
    let count = 0;
    group.querySelectorAll(".setting-row").forEach(row => { row.hidden = !row.dataset.search.includes(query); if (!row.hidden) count++; });
    group.hidden = count === 0; visible += count;
    if (query && count) group.open = true;
    group.querySelector(".group-count").textContent = String(count);
  });
  byId("config-summary").textContent = query ? "Найдено настроек: " + visible : "Доступно настроек: " + state.settings.size + ". Секреты и служебные параметры в панели не отображаются.";
}
function updateConfigSummary() {
  const count = dirtySettings().length;
  const config = state.config;
  const pending = config?.pending?.length || 0;
  byId("nav-dirty").hidden = !count;
  byId("nav-dirty").textContent = String(count);
  byId("dirty-summary").textContent = count ? "Несохранённых изменений: " + count : "Нет несохранённых изменений";
  byId("pending-summary").textContent = pending ? "Сохранено, но не применяется: " + pending : "";
  byId("discard-config").disabled = !count || state.configBusy;
  byId("save-config").disabled = !count || state.configBusy || config?.restart_scheduled;
  byId("reload-config").disabled = !pending || config?.restart_required || state.configBusy || config?.restart_scheduled;
  byId("restart-config").disabled = !config || state.configBusy || config.restart_scheduled;
  byId("confirm-restart").disabled = state.configBusy;
  byId("refresh-config").disabled = state.configBusy;
  let text = "Сохранённые настройки уже действуют.";
  if (config?.restart_scheduled) text = "Перезапуск запланирован. Ожидаем новый процесс Bridge.";
  else if (pending) text = "Ожидают применения: " + pending + ". " + (config.restart_required ? "Нужен перезапуск Bridge." : "Можно применить без перезапуска.");
  else if (config?.restart_required) text = "Сохранённые настройки требуют перезапуска Bridge.";
  byId("overview-config").textContent = config ? text : "Настройки ещё не загружены.";
  byId("apply-help").textContent = text + (count ? " Несохранённые правки при этом не применяются." : "");
  const model = state.settings.get("llm_model");
  byId("overview-model").textContent = model ? String(model.active || "Не задана") : "Настройки модели недоступны";
}
async function loadConfig(reportError = true) {
  try {
    const config = await request("api/config");
    const restartFinished = state.config?.restart_scheduled && !config.restart_scheduled;
    if (config.restart_scheduled && !state.config?.restart_scheduled) state.restartRun = state.status?.run || "";
    state.config = config;
    state.settings = new Map((config.settings || []).map(setting => [setting.path, setting]));
    renderSettings();
    if (restartFinished) {
      const confirmed = state.restartRun && state.status?.run && state.restartRun !== state.status.run;
      message("config-message", confirmed ? "Новый процесс Bridge на связи. Состояние настроек обновлено." : "Bridge на связи; перезапуск не подтверждён. Проверьте действующие настройки и журнал.");
      state.restartRun = "";
    }
    return true;
  } catch (error) {
    if (reportError) message("config-message", error.message, true);
    if (!state.config) { byId("overview-config").textContent = "Не удалось загрузить настройки."; byId("config-summary").textContent = "Каталог настроек недоступен. Нажмите «Обновить значения», чтобы повторить."; byId("overview-model").textContent = "Настройки недоступны"; }
    return false;
  }
}
function lockSettings(locked) {
  state.configBusy = locked;
  document.querySelectorAll("#settings-fields [data-path]").forEach(control => { control.disabled = locked || state.settings.get(control.dataset.path)?.writable === false; });
  updateConfigSummary();
}
async function saveConfig(event) {
  event.preventDefault();
  if (state.configBusy) return;
  const dirty = dirtySettings();
  if (!dirty.length) return;
  if (dirty.length > 32) { message("config-message", "За один раз можно сохранить до 32 настроек. Сохраните изменения меньшими группами.", true); return; }
  const changes = {};
  try { for (const setting of dirty) changes[setting.path] = parseValue(setting, state.drafts.get(setting.path)); }
  catch (error) { message("config-message", error.message, true); return; }
  lockSettings(true); message("config-message", "Сохраняем настройки…");
  try {
    const result = await post("api/config", { changes });
    state.config = { ...state.config, ...result };
    for (const setting of dirty) state.drafts.delete(setting.path);
    await loadConfig(false);
    message("config-message", "Настройки сохранены в файл. " + (result.restart_required ? "Для применения нужен перезапуск Bridge." : result.pending?.length ? "Теперь примените изменения без перезапуска." : "Изменений для применения нет."));
  } catch (error) { message("config-message", error.message, true); }
  finally { lockSettings(false); }
}
async function applyConfig(action) {
  if (state.configBusy) return;
  lockSettings(true);
  try {
    const result = await post("api/apply", { action });
    byId("restart-confirmation").hidden = true;
    if (result.status === "scheduled" || result.restart_scheduled) state.restartRun = state.status?.run || "";
    state.config = { ...state.config, ...result, restart_scheduled: result.status === "scheduled" || result.restart_scheduled };
    if (result.status === "scheduled" || result.restart_scheduled) {
      message("config-message", "Перезапуск запланирован. Дождитесь повторного подключения панели.");
      setConnected(false);
    } else {
      message("config-message", "Сохранённые настройки применены.");
      await loadConfig(false); await loadStatus();
    }
  } catch (error) { message("config-message", error.message, true); }
  finally { lockSettings(false); }
}
function queryText() {
  const text = byId("query-text").value.trim();
  if (!text) throw new Error("Введите фразу для проверки.");
  if ([...text].length > 8000) throw new Error("Сократите запрос до 8000 символов.");
  return text;
}
async function previewQuery(event) {
  event.preventDefault();
  if (state.queryBusy) return;
  let text;
  try { text = queryText(); } catch (error) { message("query-message", error.message, true); return; }
  state.queryBusy = true; byId("preview-query").disabled = true; byId("execute-query").disabled = true;
  message("query-message", "Проверяем правила…");
  try {
    const result = await post("api/route", { text });
    byId("route-result").replaceChildren(...[["Действующие настройки", result.active], ["Сохранённые настройки", result.saved]].map(([label, decision]) => {
      const card = element("section", "surface");
      card.append(element("h3", "", label), element("p", "route-name", routeName(decision?.route)));
      card.append(element("p", "route-explanation", decision?.rule ? "Правило: " + decision.rule : sourceNames[decision?.source] || "Источник: " + (decision?.source || "не определён")));
      return card;
    }));
    byId("route-result").hidden = false;
    message("query-message", "Маршрут проверен без выполнения запроса. Классификатор модели не вызывался. Проверка охватывает правила и команды управления Bridge; специальные команды музыки и контекст диалога здесь не моделируются.");
  } catch (error) { message("query-message", error.message, true); }
  finally { state.queryBusy = false; byId("preview-query").disabled = false; byId("execute-query").disabled = state.status?.capabilities?.query === false; }
}
async function executeQuery() {
  if (state.queryBusy) return;
  let text;
  try { text = queryText(); } catch (error) { message("query-message", error.message, true); return; }
  state.queryBusy = true; byId("execute-query").disabled = true; byId("preview-query").disabled = true;
  message("query-message", "Bridge выполняет запрос. Дождитесь ответа…");
  byId("answer-section").hidden = true;
  try {
    const result = await post("api/query", { text }, 180000);
    byId("answer-text").textContent = result.answer || (result.route === "yandex" ? "Запрос направлен Алисе. Вне голосовой сессии колонки этот маршрут может завершиться без текстового ответа. Для проверки ответа модели используйте фразу, которая направляется в модель." : "Bridge завершил запрос без текстового ответа.");
    byId("answer-meta").textContent = routeName(result.route) + (typeof result.elapsed_ms === "number" ? " · " + (result.elapsed_ms / 1000).toLocaleString("ru-RU", { maximumFractionDigits: 2 }) + " с" : "");
    byId("answer-section").hidden = false;
    message("query-message", "Запрос выполнен.");
    await loadStatus();
    await loadConfig(false);
  } catch (error) { message("query-message", error.message, true); }
  finally { state.queryBusy = false; byId("execute-query").disabled = state.status?.capabilities?.query === false; byId("preview-query").disabled = false; }
}
async function airPlayAction(action) {
  if (state.airplayBusy) return;
  state.airplayBusy = true; renderAirPlay(state.status?.airplay); message("airplay-message", "Отправляем команду…");
  try {
    const result = await post("api/airplay", { action });
    if (state.status) state.status.airplay = result.airplay;
    message("airplay-message", result.message || "Команда выполнена.");
  } catch (error) { message("airplay-message", error.message, true); }
  finally { state.airplayBusy = false; renderAirPlay(state.status?.airplay); }
}
async function loadHistory(event) {
  if (event) event.preventDefault();
  if (state.historyBusy) return;
  const since = byId("history-since").value;
  const until = byId("history-until").value;
  if (since && until && since > until) { message("history-message", "Начало периода должно быть раньше окончания.", true); return; }
  const params = new URLSearchParams({ query: byId("history-search").value.trim(), limit: byId("history-limit").value });
  if (since) params.set("since", new Date(since + "T00:00:00").toISOString());
  if (until) { const end = new Date(until + "T00:00:00"); end.setDate(end.getDate() + 1); params.set("until", end.toISOString()); }
  state.historyBusy = true; byId("search-history").disabled = true; message("history-message", "Ищем записи…");
  try {
    const result = await request("api/history?" + params);
    const items = result.items || [];
    byId("history-summary").textContent = "Записей: " + items.length + ". Время и даты фильтра: " + Intl.DateTimeFormat().resolvedOptions().timeZone + " (часовой пояс браузера)." + (result.partial ? " Показана часть истории: достигнут предел чтения." : "");
    byId("history-items").replaceChildren(...items.map(item => {
      const entry = element("article", "history-item"); const meta = element("div", "history-meta");
      const time = element("time", "", timeText(item.time)); time.dateTime = item.time; meta.append(time);
      if (item.route) meta.append(element("span", "badge", routeName(item.route)));
      entry.append(meta, element("h3", "history-query", item.query || "Запрос без текста"), element("p", "history-answer", item.answer || "Ответ не записан."));
      return entry;
    }));
    if (!items.length) byId("history-items").append(element("p", "empty-state", "Записей не найдено. Измените текст поиска или период."));
    message("history-message", result.recording ? "" : "Запись новых диалогов отключена. Сохранённая ранее история остаётся доступна. Включить запись можно в настройках.");
  } catch (error) { message("history-message", error.message, true); if (!byId("history-items").querySelector("article")) byId("history-items").replaceChildren(element("p", "empty-state", "История недоступна. Повторите поиск после восстановления связи.")); }
  finally { state.historyBusy = false; byId("search-history").disabled = false; }
}
function readableLogs(raw) {
  const query = byId("log-search").value.trim().toLocaleLowerCase("ru-RU");
  const levelFilter = byId("log-level").value;
  return raw.split("\n").filter(Boolean).map(line => {
    try {
      const { time, level, msg, ...fields } = JSON.parse(line);
      const details = Object.entries(fields).map(([key, value]) => key + "=" + JSON.stringify(value));
      return { level: String(level || "").toUpperCase(), text: [time ? new Date(time).toLocaleTimeString("ru-RU") : "", level, msg, ...details].filter(value => value !== undefined).join(" ") };
    } catch (_) { return { level: "", text: line }; }
  }).filter(line => (!levelFilter || line.level === levelFilter) && (!query || line.text.toLocaleLowerCase("ru-RU").includes(query))).map(line => line.text).join("\n");
}
function renderLogs() {
  const log = byId("log");
  const atEnd = log.scrollHeight - log.scrollTop - log.clientHeight < 48;
  const readable = readableLogs(logState.text);
  log.textContent = readable || (logState.text ? "По выбранным фильтрам событий нет." : "Событий пока нет. Повторите действие на колонке.");
  if (atEnd) log.scrollTop = log.scrollHeight;
}
async function loadLogs() {
  if (logState.paused || logState.busy || document.hidden || state.page !== "logs") return;
  logState.busy = true;
  try {
    const data = await request("logs?" + new URLSearchParams({ after: String(logState.cursor), run: logState.run }), {}, 5000);
    if (logState.paused) return;
    if (data.reset || logState.run !== data.run) logState.text = "";
    logState.run = data.run; logState.cursor = data.cursor; logState.text += data.text || "";
    if (logState.text.length > 1048576) { logState.text = logState.text.slice(-1048576); logState.text = logState.text.slice(logState.text.indexOf("\n") + 1); }
    renderLogs();
    if (data.version) byId("version").textContent = "Версия " + data.version;
    byId("limit").textContent = data.truncated ? "Начало журнала уже вышло за лимит 1 МиБ. Скачайте файл сейчас; полный сервисный журнал остаётся на колонке." : "Доступны события с запуска Bridge, до 1 МиБ. Файл содержит журнал, версию и текущее состояние AirPlay.";
    byId("connection").classList.remove("error");
    byId("connection").textContent = "Обновлено в " + new Date().toLocaleTimeString("ru-RU") + ". Новые события появляются автоматически.";
  } catch (error) { if (!logState.paused) { byId("connection").classList.add("error"); byId("connection").textContent = error.message + " Панель попробует снова."; } }
  finally { logState.busy = false; }
}
function navigate() {
  const requested = location.hash.slice(1);
  const page = Object.hasOwn(pageNames, requested) ? requested : "overview";
  state.page = page;
  for (const id of Object.keys(pageNames)) byId(id).hidden = id !== page;
  document.querySelectorAll(".nav-link").forEach(link => { const active = link.hash === "#" + page; link.classList.toggle("active", active); if (active) link.setAttribute("aria-current", "page"); else link.removeAttribute("aria-current"); });
  byId("page-title").textContent = pageNames[page]; document.title = pageNames[page] + " — Quasar Bridge";
  if (page === "settings") loadConfig();
  if (page === "history") loadHistory();
  if (page === "logs") loadLogs();
  window.scrollTo(0, 0);
}
document.querySelector(".skip-link").addEventListener("click", event => { event.preventDefault(); byId("content").focus(); });
byId("refresh-status").addEventListener("click", () => loadStatus(true));
byId("refresh-config").addEventListener("click", () => { message("config-message"); loadConfig(); });
byId("settings-search").addEventListener("input", filterSettings);
byId("settings-form").addEventListener("submit", saveConfig);
byId("discard-config").addEventListener("click", () => { state.drafts.clear(); renderSettings(); message("config-message", "Несохранённые правки отменены."); });
byId("reload-config").addEventListener("click", () => applyConfig("reload"));
byId("restart-config").addEventListener("click", () => { byId("restart-confirmation").hidden = false; byId("confirm-restart").focus(); });
byId("cancel-restart").addEventListener("click", () => { byId("restart-confirmation").hidden = true; byId("restart-config").focus(); });
byId("confirm-restart").addEventListener("click", () => applyConfig("restart"));
byId("query-form").addEventListener("submit", previewQuery);
byId("execute-query").addEventListener("click", executeQuery);
document.querySelectorAll("[data-airplay]").forEach(button => button.addEventListener("click", () => airPlayAction(button.dataset.airplay)));
byId("volume-level").addEventListener("input", () => { byId("volume-value").value = byId("volume-level").value + "%"; });
byId("set-volume").addEventListener("click", () => airPlayAction("volume:" + byId("volume-level").value));
byId("history-form").addEventListener("submit", loadHistory);
byId("pause").addEventListener("click", () => { logState.paused = !logState.paused; byId("pause").textContent = logState.paused ? "Продолжить обновление" : "Приостановить обновление"; byId("pause").setAttribute("aria-pressed", String(logState.paused)); if (logState.paused) byId("connection").textContent = "Обновление страницы приостановлено. Bridge продолжает записывать события."; else loadLogs(); });
byId("log-search").addEventListener("input", renderLogs);
byId("log-level").addEventListener("change", renderLogs);
document.querySelectorAll("[data-settings-filter]").forEach(link => link.addEventListener("click", () => { byId("settings-search").value = link.dataset.settingsFilter; filterSettings(); }));
window.addEventListener("hashchange", navigate);
window.addEventListener("beforeunload", event => { if (dirtySettings().length) { event.preventDefault(); event.returnValue = ""; } });
document.addEventListener("visibilitychange", () => { if (!document.hidden) { loadStatus(); loadLogs(); } });
loadStatus();
if (location.hash !== "#settings") loadConfig();
navigate();
window.setInterval(() => { if (!document.hidden) loadStatus(); }, 5000);
window.setInterval(loadLogs, 1500);
