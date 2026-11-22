#!/usr/bin/env node
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { chromium } from "playwright";

const HOST = "127.0.0.1";
const MODEL = "demo-model";
const FIRST = "Спроси нейросеть: почему небо голубое?";
const SECOND = "А что меняется на закате?";
const ANSWERS = [
  "Молекулы воздуха сильнее рассеивают синий свет, чем красный. Поэтому рассеянный солнечный свет со всех сторон неба кажется голубым.",
  "На закате свет проходит через более толстый слой воздуха. Синий свет сильнее рассеивается по пути, а до нас доходит больше красного и оранжевого света.",
];

async function listen(server) {
  server.listen(0, HOST);
  await once(server, "listening");
  return server.address().port;
}

async function unusedPort() {
  const probe = createServer();
  const port = await listen(probe);
  await new Promise((resolve, reject) => probe.close(error => error ? reject(error) : resolve()));
  return port;
}

async function checkTheme(page, output) {
  const picker = page.getByRole("combobox", { name: "Тема", exact: true });
  const expectScheme = scheme => page.waitForFunction(expected => getComputedStyle(document.documentElement).colorScheme === expected, scheme);
  assert.equal(await picker.inputValue(), "system");
  await page.emulateMedia({ colorScheme: "dark" });
  await expectScheme("dark");
  await page.screenshot({ path: path.join(output, "web-panel-overview-dark.png"), animations: "disabled" });

  await picker.selectOption("light");
  await page.reload();
  await expectScheme("light");
  assert.equal(await picker.inputValue(), "light");
  await picker.selectOption("system");
  await expectScheme("dark");
  await page.emulateMedia({ colorScheme: "light" });
  await expectScheme("light");
  await picker.selectOption("dark");
  await expectScheme("dark");
}

async function checkRouting(page, output) {
  await page.getByRole("link", { name: "Маршрутизация", exact: true }).click();
  await page.getByRole("button", { name: "Добавить правило" }).click();
  const editor = page.getByRole("dialog");
  const preview = editor.getByRole("form", { name: "Проверить это правило" });
  const phrase = preview.getByLabel("Пробная фраза");
  const wake = preview.getByLabel("ID слова активации");
  const result = preview.getByRole("status");
  const revision = async () => (await (await page.request.get(new URL("api/routing", page.url()).href)).json()).saved_revision;
  const initialRevision = await revision();
  await editor.getByLabel("Обработчик").selectOption("home");
  await editor.getByLabel("Содержит любую из фраз").fill("свет");
  await editor.locator("#rule-extra summary").click();
  await editor.getByLabel("ID слов активации").fill("demo");
  await editor.getByRole("button", { name: "Добавить группу" }).click();
  await editor.getByLabel("Любая фраза из группы").fill("мастерской");
  await phrase.fill("Включи свет в мастерской");
  await phrase.press("Enter");
  await result.filter({ hasText: "Фраза не подходит" }).waitFor();
  await wake.fill("demo");
  assert.ok(await result.isHidden());
  await preview.getByRole("button", { name: "Проверить" }).click();
  await result.filter({ hasText: "Условия совпали. Обработчик — Умный дом." }).waitFor();
  assert.equal(await revision(), initialRevision);
  await editor.getByLabel("Название").fill("Свет в мастерской");
  await editor.getByRole("button", { name: "Сохранить и применить" }).click();
  await editor.waitFor({ state: "hidden" });
  const test = page.locator("#routing-test");
  await test.getByLabel("Пробная фраза").fill("Включи свет в мастерской");
  await test.getByLabel("ID слова активации").fill("demo");
  const check = async expected => {
    const response = page.waitForResponse(r => r.url().endsWith("/api/route"));
    await test.getByRole("button", { name: "Проверить", exact: true }).click();
    const result = await (await response).json();
    assert.equal(result.active.rule, expected);
    assert.deepEqual(result.active, result.saved);
    assert.equal(result.classifier_called, false);
    await page.locator("#routing-result").waitFor();
  };
  await test.getByLabel("ID слова активации").fill("another");
  await check("home_devices");
  await test.getByLabel("ID слова активации").fill("demo");
  await check("Свет в мастерской");
  await page.screenshot({ path: path.join(output, "web-panel-routing.png"), animations: "disabled" });
  await page.locator("#routing-live summary").click();
  await page.getByRole("button", { name: "Определить маршрут" }).click();
  await page.locator("#routing-live-result").filter({ hasText: "Алиса · Home Assistant отключён · Свет в мастерской" }).waitFor();
  await page.getByRole("button", { name: "Изменить правило Свет в мастерской" }).click();
  assert.equal(await phrase.inputValue(), "");
  assert.ok(await result.isHidden());
  const savedRevision = await revision();
  await editor.getByLabel("Правило включено").uncheck();
  await editor.getByLabel("Содержит любую из фраз").fill("вентилятор");
  await phrase.fill("Включи свет в мастерской");
  await wake.fill("demo");
  await wake.press("Enter");
  await result.filter({ hasText: "Фраза не подходит" }).waitFor();
  await editor.getByLabel("Содержит любую из фраз").fill("свет");
  await editor.getByLabel("Регулярное выражение Go").fill("[");
  await phrase.press("Enter");
  await result.filter({ hasText: "Проверьте синтаксис" }).waitFor();
  await editor.getByLabel("Регулярное выражение Go").fill("");
  await phrase.press("Enter");
  await result.filter({ hasText: "Условия совпали" }).filter({ hasText: "Правило выключено" }).waitFor();
  assert.equal(await revision(), savedRevision);
  await editor.getByRole("button", { name: "Сохранить и применить" }).click();
  await editor.waitFor({ state: "hidden" });
  await check("home_devices");
  await page.getByRole("button", { name: "Изменить правило Свет в мастерской" }).click();
  await editor.getByLabel("Правило включено").check();
  await editor.getByLabel("Место в списке").selectOption("");
  await editor.getByRole("button", { name: "Сохранить и применить" }).click();
  await editor.waitFor({ state: "hidden" });
  await check("home_devices");
  await page.getByRole("button", { name: "Изменить правило Свет в мастерской" }).click();
  page.once("dialog", dialog => dialog.accept());
  await editor.getByRole("button", { name: "Удалить правило…" }).click();
  await editor.waitFor({ state: "hidden" });
  await page.getByRole("button", { name: "Изменить правило Свет в мастерской" }).waitFor({ state: "detached" });
}

async function checkAccount(page) {
  await page.getByRole("link", { name: "ChatGPT", exact: true }).click();
  await page.getByText("Вход не выполнен", { exact: true }).waitFor();
  // Exercise the real cross-site navigation without contacting a provider.
  await page.route("https://auth.openai.com/**", async route => {
    const q = new URL(route.request().url()).searchParams;
    const callback = new URL(q.get("redirect_uri"));
    callback.search = new URLSearchParams({ state: q.get("state"), error: "access_denied" });
    await route.fulfill({ contentType: "text/html", body: '<a href="' + callback.href.replaceAll("&", "&amp;") + '">Decline</a>' });
  });
  await page.getByRole("button", { name: "Войти через ChatGPT" }).click();
  await page.getByRole("link", { name: "Decline" }).click();
  await page.locator("#result").filter({ hasText: "Вход отменён" }).waitFor();
  assert.equal(new URL(page.url()).search, "");
  assert.equal(await page.locator("html").evaluate(root => getComputedStyle(root).colorScheme), "dark");
  await page.getByRole("link", { name: "Вернуться в панель" }).click();
  await page.getByText("Вход не выполнен", { exact: true }).waitFor();

  // Present a second loopback Host to simulate a panel opened by a remote
  // browser. The real API selects the manual return and validates its state.
  await page.route("**/api/chatgpt/start", async route => {
    const forwarded = new URL(route.request().url());
    forwarded.hostname = "127.0.0.2";
    const response = await route.fetch({ headers: { ...route.request().headers(), host: forwarded.host, origin: forwarded.origin } });
    await route.fulfill({ response });
  });
  await page.getByRole("button", { name: "Войти через ChatGPT" }).click();
  const accountLink = page.getByRole("link", { name: "Открыть ChatGPT в новой вкладке" });
  await accountLink.waitFor();
  const authorization = new URL(await accountLink.getAttribute("href"));
  const callback = new URL(authorization.searchParams.get("redirect_uri"));
  assert.equal(callback.port, "1455");
  callback.search = new URLSearchParams({ state: authorization.searchParams.get("state"), error: "access_denied" });
  await page.getByLabel("Адрес после входа").fill(callback.href);
  await page.getByRole("button", { name: "Завершить вход" }).click();
  await page.locator("#account-message").filter({ hasText: "Вход отменён" }).waitFor();
  assert.equal(await page.getByLabel("Адрес после входа").inputValue(), "");
}

async function capture(binary, output) {
  const temp = await mkdtemp(path.join(tmpdir(), "quasar-web-screenshots-"));
  const conversations = [];
  let backendError;
  let bridge;
  let browser;
  const backend = createServer(async (request, response) => {
    try {
      assert.equal(request.method, "POST");
      assert.equal(request.url, "/v1/chat/completions");
      const chunks = [];
      for await (const chunk of request) chunks.push(chunk);
      const { model, messages } = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      assert.equal(model, MODEL);
      const prompts = messages.filter(message => message.role === "user").map(message => message.content);
      const turn = conversations.length;
      assert.match(prompts[0], /почему небо голубое\?/i);
      assert.equal(prompts.length, turn + 1);
      if (turn === 1) assert.equal(prompts[1], SECOND);
      assert.ok(ANSWERS[turn], "unexpected extra model request");
      conversations.push(prompts);
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify({
        id: `demo-${turn + 1}`, object: "chat.completion", model: MODEL,
        choices: [{ index: 0, message: { role: "assistant", content: ANSWERS[turn] }, finish_reason: "stop" }],
      }));
    } catch (error) {
      backendError = error;
      response.writeHead(400).end();
    }
  });

  try {
    const mockPort = await listen(backend);
    const bridgePort = await unusedPort();
    let panelPort = await unusedPort();
    while (panelPort === bridgePort) panelPort = await unusedPort();
    const config = path.join(temp, "config.json");
    const credentials = path.join(temp, "account.json");
    await writeFile(config, JSON.stringify({
      listen: `${HOST}:${bridgePort}`,
      upstream: `ws://${HOST}:${mockPort}/uni.ws`,
      mode: "hybrid",
      diagnostics: { port: panelPort, network_access: false },
      llm_base_url: `http://${HOST}:${mockPort}/v1`,
      llm_model: MODEL,
      airplay: { disabled: true },
      assistant_tools: { enabled: false },
      conversation_log: { enabled: false },
      routing: { default_route: "llm" },
    }), { mode: 0o600 });
    bridge = spawn(binary, ["-config", config], { cwd: temp, env: { ...process.env, QUASAR_LLM_CHATGPT_AUTH_FILE: credentials }, stdio: ["ignore", "inherit", "inherit"] });
    const spawned = once(bridge, "spawn");
    await spawned;

    const url = `http://${HOST}:${panelPort}/diagnostics/`;
    let ready = false;
    for (let attempt = 0; attempt < 50; attempt++) {
      assert.equal(bridge.exitCode, null, "Bridge exited before serving its diagnostics");
      const result = await fetch(url + "api/status", { signal: AbortSignal.timeout(1000) }).catch(() => undefined);
      if (result?.ok && (await result.json()).status === "ok") {
        ready = true;
        break;
      }
      await delay(100);
    }
    assert.ok(ready, "Bridge diagnostics did not become ready");

    await mkdir(output, { recursive: true });
    browser = await chromium.launch({ headless: true });
    const page = await browser.newPage({ viewport: { width: 1040, height: 925 }, deviceScaleFactor: 1, locale: "ru-RU", timezoneId: "UTC" });
    const pageErrors = [];
    page.on("pageerror", error => pageErrors.push(error.message));
    await page.goto(url);
    await page.locator("#overview-model").filter({ hasText: MODEL }).waitFor();
    await page.screenshot({ path: path.join(output, "web-panel-overview.png"), animations: "disabled" });

    await checkTheme(page, output);
    await checkRouting(page, output);
    await checkAccount(page);
    await page.setViewportSize({ width: 390, height: 844 });
    for (const section of ["overview", "routing", "account"]) {
      await page.goto(url + "#" + section);
      await page.locator("#" + section).waitFor();
      assert.equal(await page.getByRole("combobox", { name: "Тема", exact: true }).inputValue(), "dark");
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), section + " exceeds mobile width");
    }
    await page.getByRole("combobox", { name: "Тема", exact: true }).selectOption("system");
    await page.setViewportSize({ width: 1040, height: 1150 });
    await page.getByRole("link", { name: "Чат", exact: true }).click();
    const input = page.getByRole("textbox", { name: "Сообщение" });
    for (const [index, prompt] of [FIRST, SECOND].entries()) {
      await input.fill(prompt);
      const received = page.waitForResponse(response => response.url() === url + "api/query");
      await input.press("Enter");
      const response = await received;
      if (backendError) throw backendError;
      assert.ok(response.ok(), `query returned HTTP ${response.status()}`);
      assert.equal((await response.json()).route, "llm");
      await page.getByText(ANSWERS[index], { exact: true }).waitFor();
    }
    assert.equal(conversations.length, 2);
    const messages = page.locator("#chat-messages .chat-message");
    assert.equal(await messages.count(), 4);
    await page.getByRole("heading", { name: "Чат", exact: true }).click();
    const insideViewport = await messages.evaluateAll(items => {
      const viewport = document.querySelector("#chat-scroll").getBoundingClientRect();
      return items.every(item => {
        const bounds = item.getBoundingClientRect();
        return bounds.top >= viewport.top && bounds.bottom <= viewport.bottom;
      });
    });
    assert.ok(insideViewport, "capture would crop a chat message");
    await page.mouse.move(5, 5);
    await page.screenshot({ path: path.join(output, "web-panel-chat.png"), animations: "disabled" });
    assert.deepEqual(pageErrors, []);
  } finally {
    await browser?.close();
    if (bridge?.pid && bridge.exitCode === null && bridge.signalCode === null) {
      const exited = once(bridge, "exit");
      bridge.kill();
      await exited;
    }
    backend.closeAllConnections();
    await new Promise(resolve => backend.close(resolve));
    await rm(temp, { recursive: true, force: true });
  }
}

const [, , binary, output] = process.argv;
if (!binary || !output || process.argv.length !== 4) {
  console.error("Usage: node capture.mjs <quasar-bridge-binary> <output-directory>");
  process.exitCode = 2;
} else {
  await capture(path.resolve(binary), path.resolve(output));
}
