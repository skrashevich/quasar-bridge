import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { chromium } from "playwright";
import { declineAccountLogin } from "./account.mjs";

const [scenario, url, failureImage] = process.argv.slice(2);
const nav = (page, name) => page.getByRole("navigation").getByRole("link", { name, exact: true }).click();
async function api(page, path) {
  const response = await page.request.get(new URL("api/" + path, url).href);
  assert.equal(response.status(), 200, path);
  return response.json();
}

async function overview(page) {
  await page.getByText("Bridge на связи", { exact: true }).waitFor();
  await page.locator("#overview-model").filter({ hasText: /^kuyase-fixture$/ }).waitFor();
  const status = await api(page, "status");
  assert.equal(status.mode, "hybrid");
  assert.equal(await page.locator("#overview-mode").textContent(), "Гибридный режим");
  const speaker = status.speaker || {};
  const names = { yandexmidi: "Яндекс Станция 2", yandexmini: "Яндекс Станция Мини", yandexmicro: "Яндекс Станция Лайт", yandexmini_2: "Яндекс Станция Мини 2", cucumber: "Яндекс Станция Миди" };
  assert.equal(await page.locator("#station-model").textContent(), names[speaker.model] || speaker.model || "Нет данных");
  assert.equal(await page.locator("#station-serial").textContent(), speaker.serial_number || "Нет данных");
  assert.equal(await page.locator("#station-firmware").textContent(), speaker.firmware_version || "Нет данных");
  const theme = page.getByRole("combobox", { name: "Тема", exact: true });
  await page.emulateMedia({ colorScheme: "light" });
  for (const [choice, expected] of [["dark", "dark"], ["system", "light"]]) {
    await theme.selectOption(choice);
    await page.reload();
    assert.equal(await theme.inputValue(), choice);
    await page.waitForFunction(value => getComputedStyle(document.documentElement).colorScheme === value, expected);
  }
}

async function routing(page) {
  await nav(page, "Маршрутизация");
  const name = "Правило КУЯСЭ";
  const phrase = "эксперимент куясэ";
  const active = page.locator("#routing-test");
  async function route() {
    await active.getByLabel("Пробная фраза").fill(phrase);
    const [response] = await Promise.all([
      page.waitForResponse(r => r.url().endsWith("/api/route")),
      active.getByRole("button", { name: "Проверить", exact: true }).click()
    ]);
    assert.equal(response.status(), 200);
    const result = await response.json();
    assert.deepEqual(result.active, result.saved);
    await page.locator("#routing-result").waitFor();
    return result.active;
  }
  const original = await route();
  const revision = (await api(page, "routing")).saved_revision;
  await page.getByRole("button", { name: "Добавить правило" }).click();
  const editor = page.getByRole("dialog");
  await editor.getByLabel("Название").fill(name);
  await editor.getByLabel("Обработчик").selectOption("llm");
  await editor.getByLabel("Содержит любую из фраз").fill(phrase);
  const preview = editor.getByRole("form", { name: "Проверить это правило" });
  for (const [input, expected] of [["другая фраза", "Фраза не подходит"], [phrase, "Условия совпали. Обработчик — Модель."]]) {
    await preview.getByLabel("Пробная фраза").fill(input);
    await preview.getByRole("button", { name: "Проверить" }).click();
    await preview.getByRole("status").filter({ hasText: expected }).waitFor();
  }
  assert.equal((await api(page, "routing")).saved_revision, revision);
  await editor.getByRole("button", { name: "Сохранить и применить" }).click();
  await editor.waitFor({ state: "hidden" });
  await page.reload();
  const matched = await route();
  assert.equal(matched.rule, name);
  assert.equal(matched.route, "llm");
  await page.locator("#routing-result").getByText(name, { exact: false }).first().waitFor();
  for (const [handler, label] of [["command", "Вызов команды · voice_note"], ["yandex", "Алиса"]]) {
    await page.getByRole("button", { name: "Изменить правило " + name }).click();
    const beforeEdit = (await api(page, "routing")).saved_revision;
    await editor.getByLabel("Обработчик").selectOption(handler);
    if (handler === "command") await editor.getByLabel("Внешняя команда").selectOption("voice_note");
    await preview.getByLabel("Пробная фраза").fill(phrase);
    await preview.getByRole("button", { name: "Проверить" }).click();
    await preview.getByRole("status").filter({ hasText: `Условия совпали. Обработчик — ${label}.` }).waitFor();
    assert.equal((await api(page, "routing")).saved_revision, beforeEdit);
    await editor.getByRole("button", { name: "Сохранить и применить" }).click();
    await editor.waitFor({ state: "hidden" });
    const updated = await route();
    assert.equal(updated.rule, name);
    assert.equal(updated.route, handler);
    assert.equal(updated.command, handler === "command" ? "voice_note" : undefined);
    await page.locator("#routing-live summary").click();
    await page.getByRole("button", { name: "Определить маршрут" }).click();
    await page.locator("#routing-live-result").filter({ hasText: label }).filter({ hasText: name }).waitFor();
  }
  await page.getByRole("button", { name: "Изменить правило " + name }).click();
  page.once("dialog", d => d.accept());
  await editor.getByRole("button", { name: "Удалить правило…" }).click();
  await editor.waitFor({ state: "hidden" });
  assert.deepEqual(await route(), original);
}

async function settings(page) {
  await nav(page, "Настройки");
  const key = "conversation_log.max_backups";
  const current = async () => (await api(page, "config")).settings.find(s => s.path === key);
  const original = (await current()).active;
  await page.getByRole("searchbox", { name: "Поиск настроек" }).fill(key);
  const input = page.getByLabel("Число архивов", { exact: true });
  for (const value of [original === 2 ? 3 : 2, original]) {
    const before = (await current()).active;
    await input.fill(String(value));
    await page.getByRole("button", { name: "Сохранить в файл" }).click();
    await page.locator("#config-message").filter({ hasText: "Настройки сохранены в файл" }).waitFor();
    const saved = await current();
    assert.equal(saved.active, before);
    assert.equal(saved.saved, value);
    assert.equal(saved.pending, true);
    await page.getByRole("button", { name: "Применить без перезапуска" }).click();
    await page.locator("#config-message").filter({ hasText: "Сохранённые настройки применены" }).waitFor();
    const applied = await current();
    assert.equal(applied.active, value);
    assert.equal(applied.pending, false);
  }
}

async function commands(page) {
  await nav(page, "Маршрутизация");
  const example = (await api(page, "routing/command?id=voice_note")).command;
  const dialog = page.getByRole("dialog");
  const save = () => dialog.getByRole("button", { name: "Сохранить и применить" }).click();
  const createID = "web_note";
  const editedID = "web_note_edited";
  const rule = "Заметки из панели";
  const phrase = "Запись из панели купить кофе";
  const description = "Добавить в <b>блокнот</b> из панели";
  await page.getByRole("button", { name: "Добавить команду" }).click();
  assert.equal(await dialog.locator('input[type="password"]').count(), 0);
  await dialog.getByLabel("ID команды").fill(createID);
  await dialog.getByLabel("Путь к программе").fill("/bin/cat");
  await dialog.getByLabel("Когда выбирать эту команду", { exact: false }).fill(description);
  await save();
  await dialog.waitFor({ state: "hidden" });
  assert.equal((await api(page, "routing/command?id=" + createID)).command.path, "/bin/cat");
  await page.locator("#command-list").getByText(description, { exact: true }).waitFor();

  await page.getByRole("button", { name: "Добавить правило" }).click();
  await dialog.getByLabel("Название").fill(rule);
  await dialog.getByLabel("Обработчик").selectOption("command");
  await dialog.getByLabel("Внешняя команда").selectOption(createID);
  await dialog.getByLabel("Начинается с одной из фраз").fill("запись из панели");
  await save();
  await dialog.waitFor({ state: "hidden" });

  await page.getByRole("button", { name: "Изменить команду " + createID }).click();
  await dialog.getByLabel("ID команды").fill(editedID);
  await dialog.getByLabel("Путь к программе").fill(example.path);
  await dialog.getByLabel("Тайм-аут", { exact: false }).fill("2.5");
  const args = [example.args[0], "аргумент с пробелами", ""];
  for (const arg of args) {
    await dialog.getByRole("button", { name: "Добавить аргумент" }).click();
    await dialog.getByRole("textbox", { name: "Аргумент программы" }).last().fill(arg);
  }
  await save();
  await dialog.waitFor({ state: "hidden" });
  await page.reload();
  const edit = page.getByRole("button", { name: "Изменить команду " + editedID });
  await edit.click();
  await dialog.getByLabel("Путь к программе").waitFor();
  assert.equal(await dialog.getByLabel("Путь к программе").inputValue(), example.path);
  assert.deepEqual(await dialog.getByRole("textbox", { name: "Аргумент программы" }).evaluateAll(fields => fields.map(f => f.value)), args);
  const saved = await api(page, "routing/command?id=" + editedID);
  assert.deepEqual(saved.command, { path: example.path, args, timeout_ms: 2500 });
  const { active, pending } = await api(page, "routing");
  assert.equal(pending, false);
  assert.equal(active.command_ids.includes(createID), false);
  assert.equal(active.rules.find(r => r.name === rule).command, editedID);
  assert.equal(active.classifier.commands[editedID], description);
  assert.equal(active.classifier.commands[createID], undefined);
  const remove = async () => {
    page.once("dialog", d => d.accept());
    await dialog.getByRole("button", { name: "Удалить команду…" }).click();
  };
  await remove();
  await dialog.getByRole("alert").filter({ hasText: rule }).waitFor();
  await dialog.getByRole("button", { name: "Закрыть редактор команды" }).click();

  await nav(page, "Чат");
  await page.getByRole("textbox", { name: "Сообщение" }).fill(phrase);
  const [response] = await Promise.all([
    page.waitForResponse(r => r.url().endsWith("/api/query")),
    page.getByRole("button", { name: "Отправить" }).click()
  ]);
  assert.equal((await response.json()).route, "command");
  await page.locator("#chat-messages .from-assistant").getByText("Заметка сохранена.", { exact: true }).waitFor();
  await nav(page, "Маршрутизация");
  await page.getByRole("button", { name: "Изменить правило " + rule }).click();
  page.once("dialog", d => d.accept());
  await dialog.getByRole("button", { name: "Удалить правило…" }).click();
  await dialog.waitFor({ state: "hidden" });
  await edit.click();
  await remove();
  await dialog.waitFor({ state: "hidden" });
  const final = (await api(page, "routing")).active;
  assert.equal(final.command_ids.includes(editedID), false);
  assert.equal(final.classifier.commands?.[editedID], undefined);
}

async function chat(page) {
  await nav(page, "Чат");
  const messages = page.locator("#chat-messages");
  let dialog;
  for (const [question, answer] of [["первый вопрос", "Первый ответ модели."], ["а вчера?", "Ответ с учётом первого вопроса."]]) {
    const text = "спроси нейросеть: " + question;
    await page.getByRole("textbox", { name: "Сообщение" }).fill(text);
    const [response] = await Promise.all([
      page.waitForResponse(r => r.url().endsWith("/api/query")),
      page.getByRole("button", { name: "Отправить" }).click()
    ]);
    assert.equal(response.status(), 200);
    const result = await response.json();
    assert.equal(result.route, "llm");
    assert.ok(result.dialog_id);
    if (dialog) assert.equal(result.dialog_id, dialog);
    dialog = result.dialog_id;
    await messages.locator(".from-user").getByText(text, { exact: true }).waitFor();
    await messages.locator(".from-assistant").getByText(answer, { exact: true }).waitFor();
    await page.reload();
    await messages.getByText(answer, { exact: true }).waitFor();
  }
  assert.equal(await messages.locator(".chat-message").count(), 4);
  await page.getByRole("button", { name: "Новый диалог" }).click();
  await page.locator("#chat-empty").waitFor();
  assert.equal(await messages.locator(".chat-message").count(), 0);
}

async function history(page) {
  await page.setViewportSize({ width: 390, height: 844 });
  await nav(page, "История");
  await page.locator("#history-summary").filter({ hasText: /\d+ запрос/ }).waitFor();
  assert.ok(await page.locator("#history-form").isHidden());
  await page.getByRole("button", { name: "Поиск и фильтры" }).click();
  const search = page.getByRole("searchbox", { name: "Поиск по тексту" });
  await search.fill("Бавария");
  await page.getByRole("button", { name: "Найти" }).click();
  await page.locator("#history-summary").filter({ hasText: /^1 запрос ·/ }).waitFor();
  const thread = page.getByRole("region", { name: "Реплики по времени" });
  const turn = thread.locator(".history-turn");
  assert.equal(await turn.count(), 1);
  assert.match(await turn.locator(".from-user .chat-text").innerText(), /когда Бавария играет с Кёльном/);
  await turn.locator(".from-assistant").getByText("Нейросеть", { exact: true }).waitFor();
  assert.match(await turn.locator(".from-assistant .chat-text").innerText(), /https:\/\/app\.fcbayern\.com\/en\//);
  assert.ok(await thread.evaluate(el => el.scrollWidth <= el.clientWidth));
  await search.fill("нет-такого-диалога-куясэ");
  await search.press("Enter");
  await thread.getByText("Записей не найдено.", { exact: false }).waitFor();
}

async function airplay(page) {
  await nav(page, "AirPlay");
  const receiver = page.locator("#receiver");
  await receiver.filter({ hasText: /^Выключен$/ }).waitFor();
  try {
    await page.getByRole("button", { name: "Включить AirPlay", exact: true }).click();
    await receiver.filter({ hasText: /^Включён$/ }).waitFor();
    assert.equal((await api(page, "status")).airplay.state, "running");
  } finally {
    const stop = page.getByRole("button", { name: "Выключить AirPlay", exact: true });
    if (await stop.isEnabled()) await stop.click();
  }
  await receiver.filter({ hasText: /^Выключен$/ }).waitFor();
  assert.equal((await api(page, "status")).airplay.state, "stopped");
}

async function logs(page) {
  await nav(page, "Журнал событий");
  const lines = page.locator("#log .log-line:visible");
  await lines.first().waitFor();
  const pause = page.locator("#pause");
  await pause.click();
  assert.equal(await pause.getAttribute("aria-pressed"), "true");
  const search = page.getByRole("searchbox", { name: "Поиск в журнале" });
  await search.fill("нет-такого-события-куясэ");
  await page.getByText("По выбранным фильтрам событий нет.").waitFor();
  await search.fill("native_route");
  await lines.first().waitFor();
  const [download] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("link", { name: "Скачать журнал (.txt)" }).click()
  ]);
  assert.match(download.suggestedFilename(), /^quasar-bridge-diagnostics-.*\.txt$/);
  const content = await readFile(await download.path(), "utf8");
  assert.match(content, /^Quasar Bridge diagnostics\nVersion:/);
  assert.match(content, /native_route/);
}

async function account(page) {
  await nav(page, "ChatGPT");
  await page.getByText("Вход не выполнен", { exact: true }).waitFor();
  await declineAccountLogin(page);
  await page.getByRole("link", { name: "Вернуться в панель" }).click();
  await page.getByText("Вход не выполнен", { exact: true }).waitFor();
}

async function models(page) {
  await nav(page, "ChatGPT");
  await page.getByText("Аккаунт подключён", { exact: true }).waitFor();
  const picker = page.getByRole("combobox", { name: "Модель подписки ChatGPT" });
  await page.getByText("Список ещё не загружен", { exact: true }).waitFor({ state: "attached" });
  await page.getByRole("button", { name: "Обновить список" }).click();
  await picker.getByRole("option", { name: "Рассуждения (kuyase-deep)" }).waitFor({ state: "attached" });
  assert.deepEqual(await picker.locator("option").evaluateAll(opts => opts.map(o => o.value)), ["kuyase-fast", "kuyase-deep"]);
  assert.equal(await picker.inputValue(), "kuyase-fast");
  await picker.selectOption("kuyase-deep");
  await page.getByRole("button", { name: "Применить модель" }).click();
  await page.locator("#model-message").filter({ hasText: "Для следующих запросов выбрана модель kuyase-deep" }).waitFor();
  await page.reload();
  await picker.getByRole("option", { name: "Рассуждения (kuyase-deep)" }).waitFor({ state: "attached" });
  assert.equal(await picker.inputValue(), "kuyase-deep");
  assert.equal((await api(page, "status")).llm_model, "kuyase-deep");
  const model = (await api(page, "config")).settings.find(s => s.path === "llm_model");
  assert.equal(model.saved, "kuyase-deep");
  assert.equal(model.pending, false);
}

async function access(page, panelResponse) {
  const headers = panelResponse.headers();
  assert.equal(headers["cache-control"], "no-store");
  assert.equal(headers["x-content-type-options"], "nosniff");
  assert.match(headers["content-security-policy"], /frame-ancestors 'none'/);
  for (const path of ["apply", "airplay", "query", "chatgpt/start", "chatgpt/complete", "chatgpt/logout", "chatgpt/model"]) {
    const response = await page.request.get(new URL("api/" + path, url).href);
    assert.equal(response.status(), 405, path);
  }
  const config = new URL("api/config", url).href;
  const original = (await api(page, "config")).settings.find(s => s.path === "conversation_log.max_backups");
  const data = { changes: { [original.path]: original.active === 2 ? 3 : 2 } };
  for (const headers of [{ Origin: "https://example.invalid" }, { "Sec-Fetch-Site": "cross-site" }]) {
    assert.equal((await page.request.post(config, { data, headers })).status(), 403);
  }
  assert.equal((await page.request.post(config, { data: JSON.stringify(data), headers: { "Content-Type": "text/plain" } })).status(), 415);
  assert.deepEqual((await api(page, "config")).settings.find(s => s.path === original.path), original);

  const editor = new URL("api/routing/command", url);
  const before = await api(page, "routing/command");
  assert.ok(before.csrf_token);
  const command = { action: "create", id: "csrf_check", command: { path: "/bin/cat" }, expected_revision: before.revision };
  const header = { "X-Quasar-CSRF": before.csrf_token };
  for (const headers of [
    { Origin: editor.origin }, header,
    { ...header, Origin: "null" },
    { ...header, Origin: "https://example.invalid" },
    { ...header, Origin: editor.origin, "Sec-Fetch-Site": "same-site" },
  ]) {
    const result = await page.request.post(editor.href, { data: command, headers });
    assert.equal(result.status(), 403);
    assert.equal(result.headers()["access-control-allow-origin"], undefined);
  }
  const form = await page.request.post(editor.href, { form: { id: command.id }, headers: { ...header, Origin: editor.origin } });
  assert.equal(form.status(), 415);
  const preflight = await page.request.fetch(editor.href, { method: "OPTIONS", headers: {
    Origin: "https://example.invalid", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "x-quasar-csrf",
  } });
  assert.equal(preflight.headers()["access-control-allow-origin"], undefined);
  assert.equal((await api(page, "routing/command")).revision, before.revision);
  assert.ok(!(await api(page, "routing")).active.command_ids?.includes(command.id));
}

const scenarios = { overview, routing, commands, settings, chat, history, airplay, logs, account, models, access };
const browser = await chromium.launch();
try {
  const page = await browser.newPage({ locale: "ru-RU" });
  page.setDefaultTimeout(15000);
  // Keep host-side Chromium offline too. The account case registers its local consent response separately.
  await page.route("**/*", route => new URL(route.request().url()).origin === new URL(url).origin
    ? route.continue() : route.abort("blockedbyclient"));
  const errors = [];
  page.on("pageerror", e => errors.push(e.message));
  try {
    const panelResponse = await page.goto(url);
    assert.equal(panelResponse.status(), 200, "diagnostics panel");
    await scenarios[scenario](page, panelResponse);
    assert.deepEqual(errors, [], "Browser JavaScript errors");
  } catch (error) {
    await page.screenshot({ path: failureImage, timeout: 5000 }).catch(() => {});
    throw error;
  }
} finally {
  await browser.close();
}
