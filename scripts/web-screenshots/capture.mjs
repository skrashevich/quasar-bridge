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
    bridge = spawn(binary, ["-config", config], { cwd: temp, stdio: ["ignore", "inherit", "inherit"] });
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
    const page = await browser.newPage({ viewport: { width: 1040, height: 820 }, deviceScaleFactor: 1, locale: "ru-RU", timezoneId: "UTC" });
    const pageErrors = [];
    page.on("pageerror", error => pageErrors.push(error.message));
    await page.goto(url);
    await page.locator("#overview-model").filter({ hasText: MODEL }).waitFor();
    await page.screenshot({ path: path.join(output, "web-panel-overview.png"), animations: "disabled" });

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
    assert.equal(await page.locator("#chat-messages .chat-message").count(), 4);
    await page.locator("#chat-scroll").hover();
    await page.mouse.wheel(0, -1000);
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
