"use strict";

const byId = id => document.getElementById(id);
const pageNames = { overview: "Обзор", routing: "Маршрутизация", account: "ChatGPT", settings: "Настройки", query: "Чат", airplay: "AirPlay", history: "История", logs: "Журнал событий" };
const state = { page: "overview", status: null, config: null, restartRun: "", settings: new Map(), drafts: new Map(), configBusy: false, statusBusy: false, historyBusy: false, airplayBusy: false, queryBusy: false, queryDialog: storedQueryDialog(), queryMessages: [], queryExpired: false };
function storedQueryDialog() {
  try { return sessionStorage.getItem("quasarBridge.queryDialog") || ""; } catch (_) { return ""; }
}
function saveQueryDialog(id) {
  state.queryDialog = id;
  try {
    if (id) sessionStorage.setItem("quasarBridge.queryDialog", id);
    else sessionStorage.removeItem("quasarBridge.queryDialog");
  } catch (_) { /* The current tab still keeps its in-memory context. */ }
}
const logState = { cursor: 0, run: "", rows: [], paused: false, busy: false };
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
const sourceNames = { home_disabled: "Home Assistant отключён", rule: "Совпало правило", default: "Маршрут по умолчанию", classifier: "Классификатор", classifier_error: "Ошибка классификатора; выбран резервный маршрут", airplay: "Команда AirPlay", mode: "Режим Bridge", pending: "Нужен классификатор", builtin: "Встроенная команда", bridge_configuration: "Команда управления Bridge", bridge_diagnostics: "Управление веб-интерфейсом", recovery: "Команда восстановления Bridge", music: "Команда воспроизведения музыки", playback: "Управление воспроизведением" };
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
  return ({ yandex: "Алиса", llm: "Модель", home: "Умный дом", music: "Музыка", playback: "Управление воспроизведением", airplay: "AirPlay", diagnostics: "Веб-интерфейс", classify: "Классификатор", proxy: "Прокси", observe: "Наблюдение" })[route] || route || "Не определён";
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
    if (!response.ok) {
      const error = new Error(data.error || "Ошибка HTTP " + response.status);
      error.status = response.status;
      throw error;
    }
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
const stationModels = new Map([
  ["yandexstation", "Яндекс Станция"], ["yandexstation_2", "Яндекс Станция Макс"],
  ["yandexmidi", "Яндекс Станция 2"], ["orion", "Яндекс Станция 3"], ["cucumber", "Яндекс Станция Миди"],
  ["yandexmini", "Яндекс Станция Мини"], ["yandexmini_2", "Яндекс Станция Мини 2"],
  ["bergamot", "Яндекс Станция Мини 3"], ["pickle", "Яндекс Станция Мини 3 Про"],
  ["yandexmicro", "Яндекс Станция Лайт"], ["plum", "Яндекс Станция Лайт 2"],
  ["yandexplum", "Яндекс Станция Лайт 2"], ["mango", "Яндекс Станция Стрит"]
]);
function renderStatus(data) {
  state.status = data;
  setConnected(true);
  if (data.version) byId("version").textContent = "Версия " + data.version;
  byId("overview-mode").textContent = ({ hybrid: "Гибридный режим", observe: "Наблюдение", proxy: "Прокси" })[data.mode] || data.mode || "Работает";
  byId("routing-observe").hidden = data.mode !== "observe";
  const speaker = data.speaker || {};
  byId("station-model").textContent = stationModels.get(speaker.model) || speaker.model || "Нет данных";
  byId("station-serial").textContent = speaker.serial_number || "Нет данных";
  byId("station-firmware").textContent = speaker.firmware_version || "Нет данных";
  byId("frames-up").textContent = data.client_frames?.toLocaleString("ru-RU") ?? "—";
  byId("frames-down").textContent = data.server_frames?.toLocaleString("ru-RU") ?? "—";
  byId("channel-uptime").textContent = uptimeText(data.uptime_seconds);
  byId("overview-model").textContent = data.llm_model || "Модель не задана";
  const apiName = enumSettings.llm_api.find(([value]) => value === data.llm_api)?.[1] || data.llm_api || "Не указан";
  byId("overview-api").textContent = "Подключение · " + apiName;
  byId("account-provider").textContent = data.llm_api === "chatgpt"
    ? "Bridge использует ChatGPT. Выбранная модель — " + (data.llm_model || "не задана") + "."
    : "Текущее подключение — " + apiName + ". Для запросов по подписке выберите провайдер ChatGPT в установщике или конфигурации Bridge.";
  const providerLink = byId("overview-provider-link");
  providerLink.href = data.llm_api === "chatgpt" ? "#account" : "#settings";
  providerLink.textContent = data.llm_api === "chatgpt" ? "Аккаунт ChatGPT" : "Настройки модели";
  fillFacts("overview-state", [["AirPlay", data.airplay?.enabled === false ? "Отключён" : ({ running: "Включён", stopped: "Выключен", starting: "Запускается", stopping: "Выключается" })[data.airplay?.state] || "Неизвестно"], ["История диалогов", data.conversation_log_enabled ? "Записывается" : "Запись отключена"], ["Home Assistant", data.home_assistant_enabled ? "Включён" : "Отключён"]]);
  const routing = [["Включено правил", data.routing_rules ?? "—"], ["Без совпадения", data.routing_classifier_enabled ? "Выбирает классификатор" : routeName(data.routing_default_route)]];
  if (data.routing_classifier_enabled) routing.push(["При отключении классификатора", routeName(data.routing_default_route)]);
  fillFacts("routing-state", routing);
  const features = [["Инструменты модели", data.assistant_tools_enabled], ["Контекст диалога", data.dialogue_context_enabled], ["Воспроизведение музыки", data.music_playback_enabled], ["MCP-серверы", Number(data.mcp_servers_configured) > 0]];
  byId("feature-state").replaceChildren(...features.map(([name, on]) => element("span", "feature" + (on ? "" : " disabled"), name + (on ? ": включено" : ": отключено"))));
  fillFacts("technical-state", [["Время работы", uptimeText(data.uptime_seconds)], ["Кадров от колонки", data.client_frames ?? "—"], ["Кадров от Яндекса", data.server_frames ?? "—"], ["Ошибок записи истории", data.conversation_log_failures ?? "—"], ["Ревизия маршрутов", data.routing_revision || "—"], ["Панель по сети", data.diagnostics?.network_access ? "Доступна" : "Только localhost"], ["Адрес панели", data.diagnostics?.address || "—"], ["Порт панели", data.diagnostics?.port ?? "—"]]);
  byId("status-json").textContent = JSON.stringify(data, null, 2);
  renderAirPlay(data.airplay);
  setQueryBusy(state.queryBusy);
  if (data.capabilities?.query === false) byId("execute-query").title = "Обработчик запросов недоступен";
  else byId("execute-query").removeAttribute("title");
  byId("execute-help").textContent = data.capabilities?.query === false ? "Обработчик запросов недоступен. Проверьте состояние Bridge и повторите подключение." : "Ответы приходят без озвучивания. Воспроизведение имитируется; команды умного дома и изменения настроек выполняются.";
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
  const provider = state.settings.get("llm_api");
  if (path === "llm_model" && !isDirty(setting) && provider?.active === "chatgpt" && (state.drafts.get("llm_api") ?? provider.effective) === "chatgpt") {
    label.removeAttribute("for");
    help.textContent = "Выберите модель из каталога вашей подписки. Изменение вступит в силу со следующим запросом.";
    flags.replaceChildren();
    const link = element("a", "button", "Выбрать модель ChatGPT");
    link.href = "#account";
    editor.append(element("p", "small", "Действующая модель — " + (setting.active || "не задана")), link);
    row.append(description, editor);
    return row;
  }
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
  if (path === "routing.rules") {
    const link = element("a", "text-link", "Настроить и проверить правила");
    link.href = "#routing";
    const advanced = element("details", "raw-state");
    advanced.open = state.drafts.has(path);
    advanced.append(element("summary", "", "Редактировать массив JSON"), control);
    editor.append(link, advanced);
  } else if (kind !== "boolean") editor.append(control);
  const error = element("p", "field-error"); error.id = "setting-error-" + index; error.hidden = true; editor.append(error);
  control.addEventListener("input", () => {
    const rawValue = kind === "boolean" ? String(control.checked) : control.value;
    state.drafts.set(path, rawValue);
    if (kind === "boolean") control.nextElementSibling.textContent = control.checked ? "Включено" : "Выключено";
    row.classList.toggle("dirty", isDirty(setting));
    try { parseValue(setting, rawValue); error.hidden = true; control.removeAttribute("aria-invalid"); }
    catch (problem) { error.textContent = problem.message; error.hidden = false; control.setAttribute("aria-invalid", "true"); }
    updateConfigSummary();
    if (path === "llm_api") {
      renderSettings();
      byId(control.id).focus();
    }
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
    if (!state.config) { byId("overview-config").textContent = "Не удалось загрузить настройки."; byId("config-summary").textContent = "Каталог настроек недоступен. Нажмите «Обновить значения», чтобы повторить."; }
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
  if (!text) throw new Error("Введите сообщение.");
  if ([...text].length > 8000) throw new Error("Сократите запрос до 8000 символов.");
  return text;
}
function setQueryBusy(busy) {
  state.queryBusy = busy;
  byId("preview-query").disabled = busy;
  byId("decide-query").disabled = busy || state.status?.capabilities?.decide === false;
  byId("execute-query").disabled = busy || state.queryExpired || !byId("query-text").value.trim() || state.status?.capabilities?.query === false;
  byId("new-query-dialog").disabled = busy;
  byId("chat-context").textContent = busy ? "Ждём ответ…" : state.queryExpired ? "Контекст завершён — начните новый диалог" : state.queryDialog ? "Диалог продолжается" : "Новый диалог";
}
async function previewQuery(event) {
  event.preventDefault();
  if (state.queryBusy) return;
  let text;
  try { text = queryText(); } catch (error) { message("query-message", error.message, true); return; }
  setQueryBusy(true);
  message("query-message", "Проверяем правила…");
  try {
    const result = await post("api/route", { text });
    renderRouteComparison("route-result", result);
    message("query-message", "Маршрут проверен без выполнения запроса. Классификатор модели не вызывался. Проверка охватывает правила и команды управления Bridge; специальные команды музыки и контекст диалога здесь не моделируются.");
  } catch (error) { message("query-message", error.message, true); }
  finally { setQueryBusy(false); }
}
async function decideQuery(event) {
  event.preventDefault();
  if (state.queryBusy) return;
  let text;
  try { text = queryText(); } catch (error) { message("query-message", error.message, true); return; }
  setQueryBusy(true);
  byId("decision-section").hidden = true;
  message("query-message", "Bridge выбирает маршрут. При необходимости вызывается классификатор модели…");
  try {
    const result = await post("api/decide", { text }, 80000);
    byId("decision-route").textContent = routeName(result.route);
    byId("decision-source").textContent = (sourceNames[result.source] || "Источник: " + (result.source || "не определён")) + (result.rule ? ". Правило: " + result.rule : "");
    byId("decision-meta").textContent = typeof result.elapsed_ms === "number" ? (result.elapsed_ms / 1000).toLocaleString("ru-RU", { maximumFractionDigits: 2 }) + " с" : "";
    byId("decision-json").textContent = JSON.stringify(result, null, 2);
    byId("decision-section").hidden = false;
    message("query-message", "Маршрут выбран по действующим настройкам. Команда не выполнялась.");
  } catch (error) { message("query-message", error.message, true); }
  finally { setQueryBusy(false); }
}
function renderQueryPlayback(result) {
  const playback = result.playback || [];
  const report = element("details", "chat-playback");
  report.append(element("summary", "", "Подготовленные команды плееру"), element("p", "muted small", result.playback_simulated ? "Воспроизведение имитируется. Команды не отправлены плееру колонки." : "Доставка команд на колонку не подтверждена."));
  const sounds = { notification: "Сигнал уведомления", success: "Сигнал успеха", error: "Сигнал ошибки", timer: "Сигнал таймера" };
  const commands = { pause: "Пауза", stop: "Остановка воспроизведения" };
  report.append(...playback.map((request, index) => {
    const entry = element("div", "history-item");
    entry.append(element("h4", "history-query", (index + 1) + ". " + (request.title || sounds[request.sound] || commands[request.command] || "Музыка")));
    const tracks = request.tracks?.length ? request.tracks : request.url ? [{ url: request.url, title: request.title }] : [];
    if (tracks.length) {
      const sources = element("ol", "setting-values");
      for (const track of tracks) {
        const source = element("li");
        if (track.title) source.append(element("strong", "", track.title), element("br"));
        source.append(element("code", "", track.url));
        sources.append(source);
      }
      entry.append(element("p", "muted small", "Исходные аудиоссылки в порядке воспроизведения:"), sources);
    }
    return entry;
  }));
  return report;
}
function saveChat() {
  try {
    const messages = state.queryMessages.slice(-80);
    while (JSON.stringify(messages).length > 300000) messages.shift();
    sessionStorage.setItem("quasarBridge.chat", JSON.stringify({ dialog_id: state.queryDialog, messages, draft: byId("query-text").value, expired: state.queryExpired }));
  } catch (_) { /* Storage can be unavailable; the open chat remains usable. */ }
}
function sizeQueryInput() {
  const input = byId("query-text");
  input.style.height = "auto";
  input.style.height = Math.min(input.scrollHeight, 180) + "px";
  const length = [...input.value].length;
  byId("chat-count").textContent = length.toLocaleString("ru-RU") + " / 8 000";
  byId("chat-count").hidden = length < 7000;
  setQueryBusy(state.queryBusy);
}
async function copyChatText(text, button) {
  try {
    if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(text);
    else {
      const input = element("textarea", "visually-hidden", text);
      document.body.append(input); input.select();
      try { if (!document.execCommand("copy")) throw new Error("copy failed"); }
      finally { input.remove(); button.focus(); }
    }
    button.textContent = "Скопировано";
  } catch (_) { button.textContent = "Выделите текст для копирования"; }
}
function messageBubble(role, author, text, timestamp) {
  const entry = element("article", "chat-message from-" + role);
  const meta = element("div", "chat-message-meta");
  meta.append(element("strong", "", author));
  if (timestamp) {
    const time = element("time", "", new Date(timestamp).toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" }));
    time.dateTime = timestamp;
    time.title = timeText(timestamp);
    meta.append(time);
  }
  const bubble = element("div", "chat-bubble");
  bubble.append(element("p", "chat-text", text));
  entry.append(meta, bubble);
  return entry;
}
function chatMessageNode(record) {
  const author = record.role === "user" ? "Вы" : record.route === "yandex" ? "Алиса" : record.route === "llm" ? "Нейросеть" : "Bridge";
  const entry = messageBubble(record.role, author, record.text, record.time);
  if (record.status) entry.classList.add("is-" + record.status);
  if (Array.isArray(record.playback) && record.playback.length) entry.querySelector(".chat-bubble").append(renderQueryPlayback(record));
  if (record.role === "assistant" && !record.status) {
    const tools = element("div", "chat-message-tools");
    const timing = typeof record.elapsed_ms === "number" ? (record.elapsed_ms / 1000).toLocaleString("ru-RU", { maximumFractionDigits: 2 }) + " с" : "";
    tools.append(element("span", "", [routeName(record.route), timing].filter(Boolean).join(" · ")));
    const copy = element("button", "quiet", "Скопировать"); copy.type = "button";
    copy.addEventListener("click", () => copyChatText(record.text, copy)); tools.append(copy); entry.append(tools);
  }
  if (record.status === "error" && record.retry) {
    const retry = element("button", "quiet chat-retry", "Вернуть сообщение в поле ввода"); retry.type = "button";
    retry.addEventListener("click", () => { byId("query-text").value = record.retry; sizeQueryInput(); saveChat(); byId("query-text").focus(); }); entry.append(retry);
  }
  return entry;
}
function appendChatMessage(record) {
  state.queryMessages.push(record);
  const node = chatMessageNode(record);
  byId("chat-messages").append(node);
  if (state.queryMessages.length > 80) { state.queryMessages.shift(); byId("chat-messages").firstElementChild.remove(); }
  byId("chat-empty").hidden = true;
  byId("chat-scroll").scrollTop = byId("chat-scroll").scrollHeight;
  saveChat();
  return node;
}
function restoreChat() {
  try {
    const raw = sessionStorage.getItem("quasarBridge.chat");
    const saved = raw && raw.length < 400000 ? JSON.parse(raw) : null;
    if (saved?.dialog_id === state.queryDialog && Array.isArray(saved.messages)) {
      state.queryExpired = saved.expired === true;
      state.queryMessages = saved.messages.slice(-80).filter(record => record && ["user", "assistant"].includes(record.role) && typeof record.text === "string");
      for (const record of state.queryMessages) {
        if (record.status === "pending") { record.status = "error"; record.text = "Страница была закрыта до получения ответа. Результат запроса неизвестен; проверьте его перед повторной отправкой."; }
        byId("chat-messages").append(chatMessageNode(record));
      }
      byId("query-text").value = typeof saved.draft === "string" ? saved.draft.slice(0, 8000) : "";
    }
  } catch (_) { /* Ignore unavailable storage or an invalid saved transcript. */ }
  byId("chat-empty").hidden = !!state.queryMessages.length;
  sizeQueryInput();
}
async function executeQuery(event) {
  event?.preventDefault();
  if (state.queryBusy || state.queryExpired) return;
  let text;
  try { text = queryText(); } catch (error) { message("chat-feedback", error.message, true); return; }
  message("chat-feedback");
  appendChatMessage({ role: "user", text, time: new Date().toISOString() });
  byId("query-text").value = "";
  setQueryBusy(true);
  sizeQueryInput();
  const reply = { role: "assistant", text: "Ждём ответ…", status: "pending", retry: text };
  const pending = appendChatMessage(reply);
  byId("query-text").focus({ preventScroll: true });
  try {
    const payload = { text };
    if (state.queryDialog) payload.dialog_id = state.queryDialog;
    const result = await post("api/query", payload, 180000);
    saveQueryDialog(result.dialog_id || "");
    Object.assign(reply, { text: result.answer || "Запрос завершён без текстового ответа.", status: "", retry: "", route: result.route, elapsed_ms: result.elapsed_ms, time: new Date().toISOString(), playback: result.playback, playback_simulated: result.playback_simulated });
    loadStatus(); loadConfig(false);
  } catch (error) {
    state.queryExpired = error.message.includes("unknown or expired dialog_id");
    reply.status = "error";
    const errors = {
      "Diagnostic query failed; inspect the operational log": "Не удалось получить ответ. Подробности доступны в журнале событий. Перед повторной отправкой команды проверьте, выполнилась ли она.",
      "Another diagnostic query or route check is running": "Уже выполняется другой запрос или проверка маршрута. Дождитесь завершения и попробуйте снова.",
      "Text query control is unavailable": "Обработчик сообщений недоступен. Проверьте состояние Bridge.",
      "too many active query dialogs; retry after an idle dialog expires": "Достигнут лимит активных диалогов. Подождите, пока завершится один из неиспользуемых диалогов, и попробуйте снова."
    };
    reply.text = state.queryExpired ? "Контекст диалога истёк или Bridge был перезапущен. Нажмите «Новый диалог», чтобы продолжить." : errors[error.message] || error.message;
  } finally {
    const scroll = byId("chat-scroll"), nearBottom = scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 120;
    pending.replaceWith(chatMessageNode(reply));
    setQueryBusy(false); saveChat();
    if (nearBottom) scroll.scrollTop = scroll.scrollHeight;
  }
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
function renderHistory(items) {
  const list = byId("history-items");
  if (!items.length) {
    list.replaceChildren(element("p", "empty-state", "Записей не найдено. Измените текст поиска или период."));
    return;
  }
  const days = document.createDocumentFragment();
  const dateLabel = new Intl.DateTimeFormat("ru-RU", { day: "numeric", month: "long", year: "numeric" });
  let day, previousDate;
  // SearchHistory returns the most recent matches first. Read each exchange from top to bottom.
  for (const item of items.slice().reverse()) {
    const date = dateLabel.format(new Date(item.time));
    if (date !== previousDate) {
      day = element("section", "history-day");
      day.append(element("h4", "history-date", date));
      days.append(day);
      previousDate = date;
    }
    const turn = element("div", "history-turn");
    turn.append(messageBubble("user", "Пользователь", item.query, item.time));
    const author = item.route === "llm" ? "Нейросеть" : item.route ? routeName(item.route) : "Колонка";
    if (item.answer) turn.append(messageBubble("assistant", author, item.answer));
    const status = { interrupted: "Запрос прерван", incomplete: "Запись неполная", no_answer: "Текст ответа не записан" }[item.status];
    if (status || !item.answer) {
      const note = status || "Текст ответа не записан";
      turn.append(element("p", "history-note", !item.answer && item.route ? author + " · " + note : note));
    }
    day.append(turn);
  }
  list.replaceChildren(days);
  list.scrollTop = list.scrollHeight;
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
    const filtered = params.get("query") || since || until || params.get("limit") !== "20";
    byId("history-filter-toggle").textContent = filtered ? "Изменить фильтры" : "Поиск и фильтры";
    const noun = { one: "запрос", few: "запроса", many: "запросов" }[new Intl.PluralRules("ru-RU").select(items.length)];
    byId("history-summary").textContent = items.length + " " + noun + " · " + Intl.DateTimeFormat().resolvedOptions().timeZone;
    renderHistory(items);
    message("history-message", [result.partial && "Показана часть истории.", !result.recording && "Запись новых диалогов отключена в настройках."].filter(Boolean).join(" "));
  } catch (error) { message("history-message", error.message, true); if (!byId("history-items").querySelector("article")) byId("history-items").replaceChildren(element("p", "empty-state", "История недоступна. Повторите поиск после восстановления связи.")); }
  finally { state.historyBusy = false; byId("search-history").disabled = false; }
}
function packetRow(text, id, run) {
  const row = element("details", "log-packet");
  const body = element("pre", "packet-body");
  row.append(element("summary", "", text), body);
  if (id === 0) {
    body.textContent = "Пакет больше 1 МиБ. Доступна только краткая запись.";
    return row;
  }
  let loading = false;
  row.addEventListener("toggle", async () => {
    if (!row.open) { body.textContent = ""; return; }
    if (loading) return;
    loading = true;
    body.textContent = "Загружаем пакет…";
    try {
      const packet = await request("logs/" + id + "?" + new URLSearchParams({ run }), {}, 5000);
      if (row.open) body.textContent = JSON.stringify(packet, null, 2);
    } catch (error) {
      if (row.open) body.textContent = error.message;
    } finally { loading = false; }
  });
  return row;
}

const frameDirections = new Map([["client", "↑ От колонки"], ["server", "↓ От Яндекса"]]);

function logRow(line, run) {
  let text = line, level = "", direction, id;
  try {
    const { time, level: severity, msg, packet_id, ...fields } = JSON.parse(line);
    if (msg === "frame" && frameDirections.has(fields.direction)) {
      direction = fields.direction;
      delete fields.direction;
    }
    const values = Object.entries(fields).map(([key, value]) => key + "=" + JSON.stringify(value));
    text = [time ? new Date(time).toLocaleTimeString("ru-RU") : "", severity, msg, ...values].filter(v => v !== undefined).join(" ");
    level = String(severity || "").toUpperCase();
    if (msg === "frame" && Number.isSafeInteger(packet_id) && packet_id >= 0) id = packet_id;
  } catch (_) { /* Plain text log records are still searchable. */ }
  const node = id === undefined ? element("div", "log-line", text) : packetRow(text, id, run);
  if (direction) {
    const label = frameDirections.get(direction);
    node.dataset.direction = direction;
    const heading = id === undefined ? node : node.querySelector("summary");
    heading.prepend(element("span", "log-direction", label), " ");
    text += ` ${label} direction="${direction}"`;
  }
  return { node, level, search: text.toLocaleLowerCase("ru-RU") };
}

function filterLogs() {
  const query = byId("log-search").value.trim().toLocaleLowerCase("ru-RU");
  const level = byId("log-level").value;
  let visible = false;
  for (const row of logState.rows) {
    row.node.hidden = Boolean((level && row.level !== level) || (query && !row.search.includes(query)));
    if (!row.node.hidden) visible = true;
  }
  const empty = byId("log-empty");
  empty.hidden = visible;
  empty.textContent = logState.rows.length ? "По выбранным фильтрам событий нет." : "Событий пока нет. Повторите действие на колонке.";
}

const logEncoder = new TextEncoder();
function appendLogs(data) {
  const log = byId("log");
  const atEnd = log.scrollHeight - log.scrollTop - log.clientHeight < 48 && !log.querySelector("details[open]:not([hidden])");
  const reset = data.reset || logState.run !== data.run;
  if (reset) {
    log.replaceChildren(byId("log-empty"));
    logState.rows = [];
  }
  logState.run = data.run;
  logState.cursor = data.cursor;
  if (!reset && !data.text) return;
  const fragment = document.createDocumentFragment();
  const raw = data.text || "";
  let start = data.cursor - logEncoder.encode(raw).length;
  for (const line of raw.split("\n")) {
    if (!line) { start++; continue; }
    const row = logRow(line, data.run);
    row.start = start;
    start += logEncoder.encode(line).length + 1;
    logState.rows.push(row);
    fragment.append(row.node);
  }
  let expired = 0;
  while (expired < logState.rows.length && logState.rows[expired].start < data.cursor - 1048576) {
    logState.rows[expired++].node.remove();
  }
  if (expired) logState.rows.splice(0, expired);
  log.append(fragment);
  filterLogs();
  if (atEnd) log.scrollTop = log.scrollHeight;
}

async function loadLogs() {
  if (logState.paused || logState.busy || document.hidden || state.page !== "logs") return;
  logState.busy = true;
  try {
    const data = await request("logs?" + new URLSearchParams({ after: String(logState.cursor), run: logState.run }), {}, 5000);
    if (logState.paused) return;
    appendLogs(data);
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
  if (page === "routing") loadRouting();
  if (page === "account") loadAccount();
  if (page === "history") loadHistory();
  if (page === "logs") loadLogs();
  if (page === "query") {
    sizeQueryInput();
    byId("chat-scroll").scrollTop = byId("chat-scroll").scrollHeight;
  }
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
byId("query-form").addEventListener("submit", executeQuery);
byId("decide-query").addEventListener("click", decideQuery);
byId("preview-query").addEventListener("click", previewQuery);
byId("query-text").addEventListener("input", () => { sizeQueryInput(); saveChat(); byId("decision-section").hidden = true; byId("route-result").hidden = true; message("query-message"); message("chat-feedback"); });
byId("query-text").addEventListener("keydown", event => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing && event.keyCode !== 229) { event.preventDefault(); byId("query-form").requestSubmit(); }
});
document.querySelectorAll("[data-chat-prompt]").forEach(button => button.addEventListener("click", () => { byId("query-text").value = button.dataset.chatPrompt; sizeQueryInput(); saveChat(); byId("query-text").focus(); }));
byId("new-query-dialog").addEventListener("click", () => {
  if (state.queryBusy) return;
  saveQueryDialog("");
  state.queryMessages = []; state.queryExpired = false;
  byId("chat-messages").replaceChildren(); byId("chat-empty").hidden = false;
  byId("decision-section").hidden = true; byId("route-result").hidden = true;
  message("query-message"); message("chat-feedback");
  setQueryBusy(false); saveChat();
  byId("query-text").focus();
});
document.querySelectorAll("[data-airplay]").forEach(button => button.addEventListener("click", () => airPlayAction(button.dataset.airplay)));
byId("volume-level").addEventListener("input", () => { byId("volume-value").value = byId("volume-level").value + "%"; });
byId("set-volume").addEventListener("click", () => airPlayAction("volume:" + byId("volume-level").value));
byId("history-form").addEventListener("submit", loadHistory);
byId("history-filter-toggle").addEventListener("click", event => {
  const toggle = event.currentTarget;
  toggle.setAttribute("aria-expanded", String(toggle.getAttribute("aria-expanded") !== "true"));
});
byId("pause").addEventListener("click", () => { logState.paused = !logState.paused; byId("pause").textContent = logState.paused ? "Продолжить обновление" : "Приостановить обновление"; byId("pause").setAttribute("aria-pressed", String(logState.paused)); if (logState.paused) byId("connection").textContent = "Обновление страницы приостановлено. Bridge продолжает записывать события."; else loadLogs(); });
byId("log-search").addEventListener("input", filterLogs);
byId("log-level").addEventListener("change", filterLogs);
document.querySelectorAll("[data-settings-filter]").forEach(link => link.addEventListener("click", () => { byId("settings-search").value = link.dataset.settingsFilter; filterSettings(); }));
window.addEventListener("hashchange", navigate);
window.addEventListener("beforeunload", event => { if (dirtySettings().length) { event.preventDefault(); event.returnValue = ""; } });
document.addEventListener("visibilitychange", () => { if (!document.hidden) { loadStatus(); loadLogs(); } });
setupRouting();
setupAccount();
restoreChat();
loadStatus();
if (location.hash !== "#settings") loadConfig();
navigate();
window.setInterval(() => { if (!document.hidden) loadStatus(); }, 5000);
window.setInterval(loadLogs, 1500);
