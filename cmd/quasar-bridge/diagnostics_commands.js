"use strict";

const commandEditor = { id: "", revision: "", csrfToken: "", ticket: "", expiryTimer: 0, load: 0 };

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
  byId("command-loading").textContent = "Загружаем команду…";
  byId("command-loading").hidden = false;
  message("command-error");
  try {
    const view = await commandRequest(id ? "?id=" + encodeURIComponent(id) : "");
    if (load !== commandEditor.load) return;
    commandEditor.revision = view.revision;
    commandEditor.csrfToken = view.csrf_token;
    byId("command-id").value = id;
    byId("command-path").value = view.command?.path || "";
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

function showCommandConfirmation(ticket, edit) {
  commandEditor.ticket = ticket.challenge_id;
  byId("command-title").textContent = "Подтверждение команды";
  byId("command-editor").hidden = true;
  byId("command-confirm").hidden = false;
  byId("command-confirm-id").textContent = edit.id;
  byId("command-confirm-invocation").textContent = [edit.command.path, ...(edit.command.args || [])].map(arg => JSON.stringify(arg)).join(" ");
  byId("command-code").value = "";
  byId("command-submit-code").disabled = false;
  byId("command-code-help").textContent = "Код действует 90 секунд. Если пропустили число, вернитесь к команде и запросите код ещё раз.";
  clearTimeout(commandEditor.expiryTimer);
  // Use the server's remaining lifetime even if the phone clock is inaccurate.
  commandEditor.expiryTimer = setTimeout(() => {
    byId("command-submit-code").disabled = true;
    byId("command-code-help").textContent = "Время истекло. Вернитесь к команде и запросите новый код.";
  }, ticket.expires_in * 1000);
}

function leaveCommandConfirmation() {
  const id = commandEditor.ticket;
  commandEditor.ticket = "";
  byId("command-title").textContent = commandEditor.id ? "Редактирование команды" : "Новая внешняя команда";
  clearTimeout(commandEditor.expiryTimer);
  byId("command-code").value = "";
  byId("command-confirm").hidden = true;
  byId("command-editor").hidden = false;
  return id;
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
    const id = leaveCommandConfirmation();
    if (id) commandRequest("", { action: "cancel", challenge_id: id }).catch(() => {});
    commandEditor.csrfToken = "";
  });
  byId("command-confirm").addEventListener("submit", event => {
    event.preventDefault();
    const code = byId("command-code").value;
    byId("command-code").value = "";
    changeRouting("command", { action: "confirm", challenge_id: commandEditor.ticket, code });
  });
  byId("command-back").addEventListener("click", async () => {
    const id = leaveCommandConfirmation();
    message("command-error");
    dialog.inert = true;
    try {
      await commandRequest("", { action: "cancel", challenge_id: id });
    } catch (error) {
      if (error.status !== 410) message("command-error", error.message, true);
    } finally {
      dialog.inert = false;
      byId("command-path").focus();
    }
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
