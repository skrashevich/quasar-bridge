"use strict";

const routingView = { data: null, busy: false, editing: null, revision: "", testVersion: 0, previewVersion: 0 };
const defaultRuleLabels = new Map([
  ["bridge_updates", "Обновления Bridge"], ["speaker_playback", "Управление плеером"],
  ["music_from_site", "Музыка с сайта"], ["assistant_readonly", "Справка и история диалога"],
  ["tts_whisper", "Шёпот Алисы"], ["tts_markup", "Голоса и звуковые эффекты"],
  ["home_devices", "Устройства умного дома"], ["complex_queries", "Запросы к нейросети"],
  ["yandex_services", "Сервисы Алисы"]
]);
const ruleLabel = name => defaultRuleLabels.get(name) || name;
const ruleFields = [
  ["contains_any", "Содержит любую из фраз"],
  ["starts_with_any", "Начинается с одной из фраз"],
  ["contains_none", "Исключить фразы"],
  ["wake_words", "ID слов активации"],
  ["regex", "Регулярное выражение Go"]
];

function routeOptions(select) {
  select.replaceChildren(...["yandex", "llm", "home"].map(route => new Option(routeName(route), route)));
}

function showRouting() {
  const data = routingView.data;
  const locked = routingView.busy || !data || data.pending;
  byId("rule-new").disabled = locked;
  byId("refresh-routing").disabled = routingView.busy;
  byId("routing-options").querySelector("fieldset").disabled = locked;
  byId("routing-pending").hidden = !data?.pending;
  byId("rule-list").inert = locked;
  if (!data) {
    byId("rule-list").replaceChildren();
    return;
  }
  byId("classifier-enabled").checked = data.active_behavior.classifier_enabled;
  byId("default-route").value = data.active_behavior.default_route;
  showClassifierHelp();
  const rules = data.active.rules;
  const list = byId("rule-list");
  list.replaceChildren(...rules.map((rule, index) => {
    const row = element("li", "rule-row" + (rule.disabled ? " rule-off" : ""));
    const order = element("span", "rule-order", String(index + 1));
    const copy = element("div", "rule-copy");
    const summary = [];
    for (const [key, label] of ruleFields) {
      const value = rule.match[key];
      if (key === "regex" && value) summary.push("Регулярное выражение");
      else if (value?.length) summary.push(label + " " + value.slice(0, 2).map(term => "«" + term + "»").join(", ") + (value.length > 2 ? "…" : ""));
    }
    if (rule.match.contains_all_groups?.length) summary.push("Группы фраз: " + rule.match.contains_all_groups.length);
    const title = element("strong", "", ruleLabel(rule.name));
    if (defaultRuleLabels.has(rule.name)) title.append(element("small", "rule-id", rule.name));
    copy.append(title, element("span", "small muted", summary.join(" · ")));
    const target = element("span", "rule-target", routeName(rule.route));
    if (rule.disabled) target.append(element("small", "", "Выключено"));
    const edit = element("button", "quiet", "Изменить");
    edit.type = "button";
    edit.setAttribute("aria-label", "Изменить правило " + rule.name);
    edit.disabled = locked;
    edit.addEventListener("click", () => openRule(rule));
    row.append(order, copy, target, edit);
    return row;
  }));
  if (!rules.length) list.append(element("li", "empty-state", "Правил пока нет. Для всех фраз действует выбранный ниже способ маршрутизации."));
}

function showClassifierHelp() {
  byId("classifier-help").textContent = byId("classifier-enabled").checked
    ? "Если классификатор недоступен, запрос получит " + routeName(routingView.data.active.classifier.on_error) + "."
    : "Запрос без совпадения сразу получит выбранный обработчик.";
}

async function loadRouting() {
  if (routingView.busy) return;
  routingView.busy = true;
  showRouting();
  resetRouteTest();
  try {
    routingView.data = await request("api/routing");
    message("routing-message");
  } catch (error) {
    routingView.data = null;
    message("routing-message", error.message, true);
  } finally {
    routingView.busy = false;
    showRouting();
  }
}

function resetRouteTest() {
  routingView.testVersion++;
  byId("routing-result").hidden = true;
  byId("routing-live").hidden = true;
  byId("routing-live").open = false;
  message("routing-live-result");
  message("routing-test-message");
}

async function testPhrase(event) {
  event.preventDefault();
  resetRouteTest();
  const version = routingView.testVersion;
  const text = byId("routing-phrase").value.trim();
  if (!text) { message("routing-test-message", "Введите пробную фразу.", true); return; }
  message("routing-test-message", "Проверяем правила…");
  try {
    const result = await post("api/route", { text, wake_word: byId("routing-wake").value.trim() });
    if (version !== routingView.testVersion) return;
    renderRouteComparison("routing-result", result);
    byId("routing-live").hidden = state.status?.capabilities?.decide === false;
    message("routing-test-message");
  } catch (error) { if (version === routingView.testVersion) message("routing-test-message", error.message, true); }
}

async function testLiveRoute() {
  const version = routingView.testVersion;
  const button = byId("routing-decide");
  button.disabled = true;
  message("routing-live-result", "Bridge выбирает маршрут…");
  try {
    const result = await post("api/decide", { text: byId("routing-phrase").value.trim(), wake_word: byId("routing-wake").value.trim() }, 80000);
    if (version !== routingView.testVersion) return;
    const source = sourceNames[result.source] || result.source;
    message("routing-live-result", [routeName(result.route), source, result.rule && ruleLabel(result.rule)].filter(Boolean).join(" · "));
  } catch (error) { if (version === routingView.testVersion) message("routing-live-result", error.message, true); }
  finally { button.disabled = false; }
}

function renderRouteComparison(id, { active, saved }) {
  const same = active.route === saved.route && active.rule === saved.rule && active.source === saved.source;
  const versions = same ? [["Действующие и сохранённые правила", active]] : [["Действующие правила", active], ["Сохранённые правила", saved]];
  const output = byId(id);
  output.replaceChildren(...versions.map(([title, decision]) => {
    const section = element("section", "test-decision");
    let reason = sourceNames[decision.source] || decision.source;
    if (decision.rule) reason = "Совпало правило «" + ruleLabel(decision.rule) + "».";
    else if (decision.route === "classify") reason = "В реальном запросе обработчика выберет модель.";
    section.append(element("h3", "", title), element("strong", "", routeName(decision.route)), element("p", "small muted", reason));
    return section;
  }));
  output.hidden = false;
}

function addRuleGroup(values = []) {
  const row = element("div", "rule-group-row");
  const label = element("label", "", "Любая фраза из группы");
  const input = element("textarea");
  input.rows = 2;
  input.value = values.join("\n");
  // Preserve exact terms (including whitespace) when only another field changes.
  input.originalTerms = values;
  input.initialText = input.value;
  label.append(input);
  const remove = element("button", "quiet", "Удалить группу");
  remove.type = "button";
  remove.addEventListener("click", () => { row.remove(); resetRulePreview(); });
  row.append(label, remove);
  byId("rule-groups").append(row);
  return input;
}

function openRule(rule) {
  const data = routingView.data;
  routingView.editing = rule;
  routingView.revision = data.saved_revision;
  const rules = data.saved.rules;
  const index = rule ? rules.findIndex(item => item.name === rule.name) : -1;
  byId("rule-dialog-title").textContent = rule ? "Редактирование правила" : "Новое правило";
  byId("rule-name").value = rule?.name || "";
  byId("rule-name").readOnly = Boolean(rule);
  byId("rule-route").value = rule?.route || "llm";
  byId("rule-before").replaceChildren(...rules.filter(item => item.name !== rule?.name).map(item => new Option("Перед «" + ruleLabel(item.name) + "»", item.name)), new Option("В конце списка", ""));
  byId("rule-before").value = rules[index + 1]?.name || "";
  byId("rule-enabled").checked = !rule?.disabled;
  for (const [key] of ruleFields) {
    const value = rule?.match[key];
    const input = byId("condition-" + key);
    input.value = key === "regex" ? value || "" : (value || []).join("\n");
    input.initialText = input.value;
  }
  byId("rule-extra").open = Boolean(rule?.match.contains_none?.length || rule?.match.wake_words?.length || rule?.match.regex || rule?.match.contains_all_groups?.length);
  byId("rule-groups").replaceChildren();
  (rule?.match.contains_all_groups || []).forEach(addRuleGroup);
  byId("rule-delete").hidden = !rule;
  byId("rule-preview").reset();
  resetRulePreview();
  message("rule-error");
  byId("rule-dialog").showModal();
  byId(rule ? "rule-route" : "rule-name").focus();
}

function readTerms(input, original = []) {
  return input.value === input.initialText ? original : input.value.split("\n").filter(term => term.trim());
}

function readRuleDraft() {
  const original = routingView.editing;
  const name = original?.name ?? byId("rule-name").value.trim();
  const match = {};
  for (const [key] of ruleFields) {
    const input = byId("condition-" + key);
    const value = key === "regex" ? (input.value === input.initialText ? original?.match.regex || "" : input.value) : readTerms(input, original?.match[key]);
    if (value.length) match[key] = value;
  }
  const groups = [...byId("rule-groups").querySelectorAll("textarea")].map(input => readTerms(input, input.originalTerms));
  if (groups.some(group => !group.length)) throw new Error("Заполните пустую группу фраз или удалите её.");
  if (groups.length) match.contains_all_groups = groups;
  if (!Object.keys(match).some(key => key !== "contains_none")) {
    throw new Error("Добавьте фразу, начало фразы, группу, слово активации или регулярное выражение.");
  }
  return { name, route: byId("rule-route").value, disabled: !byId("rule-enabled").checked, match };
}

function resetRulePreview() {
  routingView.previewVersion++;
  message("rule-preview-result");
}

async function previewRule(event) {
  event.preventDefault();
  const version = ++routingView.previewVersion;
  try {
    const rule = readRuleDraft();
    const text = byId("rule-phrase").value.trim();
    if (!text) throw new Error("Введите пробную фразу.");
    message("rule-preview-result", "Проверяем условия…");
    const { matched } = await post("api/routing/preview", { rule, text, wake_word: byId("rule-wake").value.trim() });
    if (version !== routingView.previewVersion) return;
    let result = matched ? "Условия совпали. Обработчик — " + routeName(rule.route) + "." : "Фраза не подходит под условия правила.";
    if (rule.disabled) result += " Правило выключено и пропускается при маршрутизации.";
    message("rule-preview-result", result);
  } catch (error) {
    if (version === routingView.previewVersion) message("rule-preview-result", ruleErrorText(error), true);
  }
}

function ruleErrorText(error) {
  return error.message.includes("invalid regex") ? "Проверьте синтаксис регулярного выражения Go." : error.message;
}

function saveRule(event) {
  event.preventDefault();
  let rule;
  try { rule = readRuleDraft(); }
  catch (error) { message("rule-error", error.message, true); return; }
  if (!rule.name) { message("rule-error", "Введите название правила.", true); return; }
  if (!routingView.editing && routingView.data.saved.rules.some(item => item.name === rule.name)) {
    message("rule-error", "Такое название уже есть. Укажите другое название.", true); return;
  }
  changeRouting("rule", {
    expected_revision: routingView.revision, action: "upsert", before: byId("rule-before").value, rule
  });
}

async function changeRouting(endpoint, body) {
  if (routingView.busy) return;
  const dialog = byId("rule-dialog");
  routingView.busy = true;
  const form = dialog.open ? dialog : byId("routing-options");
  form.inert = true;
  byId("refresh-routing").disabled = true;
  byId("rule-new").disabled = true;
  byId("rule-list").inert = true;
  byId("routing-options").querySelector("fieldset").disabled = true;
  const errorId = dialog.open ? "rule-error" : "routing-message";
  message(errorId);
  let applied = false;
  try {
    await post("api/routing/" + endpoint, body);
    applied = true;
    if (dialog.open) dialog.close();
    resetRouteTest();
    routingView.data = await request("api/routing");
    message("routing-message", "Изменения сохранены и уже действуют.");
    loadConfig(false);
    loadStatus();
  } catch (error) {
    const text = error.status === 409 ? "Настройки изменились. Закройте редактор, обновите список и проверьте изменения перед повторным сохранением." : ruleErrorText(error);
    if (applied) routingView.data = null;
    message(applied ? "routing-message" : errorId, applied ? "Изменения применены. Обновите список, чтобы получить текущее состояние." : text, true);
  } finally {
    routingView.busy = false;
    form.inert = false;
    showRouting();
  }
}

function setupRouting() {
  routeOptions(byId("default-route"));
  routeOptions(byId("rule-route"));
  for (const [key, title] of ruleFields) {
    const label = element("label", "", title);
    const field = element("textarea"); field.id = "condition-" + key; field.rows = 2;
    label.append(field);
    byId(["contains_any", "starts_with_any"].includes(key) ? "rule-conditions" : "extra-conditions").append(label);
  }
  byId("routing-test").addEventListener("submit", testPhrase);
  byId("routing-decide").addEventListener("click", testLiveRoute);
  byId("routing-test").addEventListener("input", resetRouteTest);
  byId("refresh-routing").addEventListener("click", loadRouting);
  byId("rule-new").addEventListener("click", () => openRule(null));
  byId("rule-editor").addEventListener("submit", saveRule);
  byId("rule-dialog").addEventListener("input", resetRulePreview);
  byId("rule-dialog").addEventListener("close", resetRulePreview);
  byId("rule-preview").addEventListener("submit", previewRule);
  byId("rule-close").addEventListener("click", () => byId("rule-dialog").close());
  byId("rule-dialog").addEventListener("cancel", event => { if (routingView.busy) event.preventDefault(); });
  byId("group-add").addEventListener("click", () => { addRuleGroup().focus(); resetRulePreview(); });
  byId("rule-delete").addEventListener("click", () => {
    const name = routingView.editing.name;
    if (window.confirm("Удалить правило «" + name + "»? Изменение сразу вступит в силу.")) {
      changeRouting("rule", { action: "delete", name, expected_revision: routingView.revision });
    }
  });
  byId("classifier-enabled").addEventListener("change", showClassifierHelp);
  byId("routing-options").addEventListener("submit", event => {
    event.preventDefault();
    changeRouting("options", { expected_revision: routingView.data.saved_revision, classifier_enabled: byId("classifier-enabled").checked, default_route: byId("default-route").value });
  });
}
