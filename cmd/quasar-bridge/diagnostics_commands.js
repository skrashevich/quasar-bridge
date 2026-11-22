"use strict";

const commandEditor = { id: "", revision: "", csrfToken: "", load: 0 };

function showCommandList(data, locked) {
  const list = byId("command-list");
  byId("command-new").disabled = locked;
  list.inert = locked;
  list.replaceChildren();
  if (!data) return;
  const ids = data.active.command_ids || [];
  for (const id of ids) {
    const row = element("li", "command-row");
    const copy = element("div", "rule-copy");
    const rules = data.active.rules.filter(rule => rule.command === id).map(rule => ruleLabel(rule.name));
    const descriptions = data.active.classifier.commands || {};
    const usage = rules.length ? "В правилах — " + rules.join(", ") : "Доступна для назначения правилу";
    copy.append(element("strong", "", id), element("span", "small muted", Object.hasOwn(descriptions, id) ? descriptions[id] : usage));
    const edit = element("button", "quiet", "Изменить");
    edit.type = "button";
    edit.setAttribute("aria-label", "Изменить команду " + id);
    edit.addEventListener("click", () => openCommand(id));
    row.append(copy, edit);
    list.append(row);
  }
  if (!ids.length) list.append(element("li", "empty-state", "Пока нет внешних команд."));
}

function commandRequest(query, body) {
  const options = { mode: "same-origin" };
  if (body) {
    options.method = "POST";
    options.headers = { "Content-Type": "application/json", "X-Quasar-CSRF": commandEditor.csrfToken };
    options.body = JSON.stringify(body);
  }
  return request("api/routing/command" + query, options);
}

function addCommandArg(value = "") {
  const row = element("div", "command-arg");
  const field = element("textarea");
  field.rows = 1;
  field.spellcheck = false;
  field.setAttribute("aria-label", "Аргумент программы");
  field.value = value;
  field.originalArgument = value;
  field.initialText = field.value;
  const remove = element("button", "quiet", "Убрать");
  remove.type = "button";
  remove.setAttribute("aria-label", "Убрать аргумент");
  remove.addEventListener("click", () => {
    row.remove();
    byId("command-arg-add").focus();
  });
  row.append(field, remove);
  byId("command-args").append(row);
  return field;
}

async function loadCommand() {
  const load = ++commandEditor.load;
  const id = commandEditor.id;
  byId("command-loading").hidden = false;
  message("command-error");
  try {
    const view = await commandRequest(id ? "?id=" + encodeURIComponent(id) : "");
    if (load !== commandEditor.load) return;
    commandEditor.revision = view.revision;
    commandEditor.csrfToken = view.csrf_token;
    byId("command-id").value = id;
    byId("command-path").value = view.command?.path || "";
    byId("command-path-help").hidden = !view.command_directory;
    if (view.command_directory) byId("command-path").placeholder = view.command_directory + "/voice-note";
    byId("command-programs").replaceChildren(...(view.programs || []).map(path => new Option(path, path)));
    byId("command-timeout").value = view.command?.timeout_ms ? view.command.timeout_ms / 1000 : "";
    byId("command-description").value = view.description || "";
    byId("command-args").replaceChildren();
    for (const arg of view.command?.args || []) addCommandArg(arg);
    byId("command-delete").hidden = !id;
    byId("command-editor").hidden = false;
    byId(id ? "command-path" : "command-id").focus();
  } catch (error) {
    if (load !== commandEditor.load) return;
    message("command-error", error.message, true);
  } finally {
    if (load === commandEditor.load) byId("command-loading").hidden = true;
  }
}

function openCommand(id) {
  commandEditor.id = id;
  byId("command-title").textContent = id ? "Редактирование команды" : "Новая внешняя команда";
  byId("command-editor").hidden = true;
  byId("command-dialog").showModal();
  loadCommand();
}

function setupCommandEditor() {
  const dialog = byId("command-dialog");
  byId("command-new").addEventListener("click", () => openCommand(""));
  byId("command-close").addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (routingView.busy) event.preventDefault(); });
  dialog.addEventListener("close", () => {
    commandEditor.load++;
    commandEditor.csrfToken = "";
  });
  byId("command-arg-add").addEventListener("click", () => addCommandArg().focus());
  byId("command-editor").addEventListener("submit", event => {
    event.preventDefault();
    const command = { path: byId("command-path").value };
    const fields = byId("command-args").querySelectorAll("textarea");
    if (fields.length) command.args = Array.from(fields, f => f.value === f.initialText ? f.originalArgument : f.value);
    const timeout = byId("command-timeout").valueAsNumber;
    if (Number.isFinite(timeout)) command.timeout_ms = Math.round(timeout * 1000);
    const edit = { action: commandEditor.id ? "update" : "create", id: byId("command-id").value,
      command, description: byId("command-description").value, expected_revision: commandEditor.revision };
    if (commandEditor.id) edit.original_id = commandEditor.id;
    changeRouting("command", edit);
  });
  byId("command-delete").addEventListener("click", () => {
    const id = commandEditor.id;
    if (window.confirm("Удалить команду «" + id + "» из маршрутизации?")) {
      changeRouting("command", { action: "delete", id, expected_revision: commandEditor.revision });
    }
  });
}
