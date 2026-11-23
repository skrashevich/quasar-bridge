import assert from "node:assert/strict";
import { chromium } from "playwright";

const [panel, issuer] = process.argv.slice(2);
const redirect = "http://127.0.0.1:19876/callback";
const code = "never-print-code";
const api = panel + "/diagnostics/api/mcp-auth";
const browser = await chromium.launch();
try {
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  await page.clock.install();
  // No browser requests leave the fixtures. In particular, loopback is
  // intercepted, not served by a callback listener.
  await context.route(url => !url.href.startsWith(panel + "/"), async route => {
    const url = new URL(route.request().url());
    if (url.origin === issuer && url.pathname === "/authorize") {
      assert.equal(url.searchParams.get("redirect_uri"), redirect);
      assert.ok(url.searchParams.get("state"));
      const callback = new URL(redirect);
      callback.search = new URLSearchParams({ state: url.searchParams.get("state"), code, iss: issuer });
      return route.fulfill({
        contentType: "text/html; charset=utf-8",
        body: `<a href="${callback.href.replaceAll("&", "&amp;")}">Подтвердить доступ</a>`,
      });
    }
    if (url.origin + url.pathname === redirect) {
      return route.fulfill({ status: 200, body: "Loopback callback intercepted for the browser test" });
    }
    await route.abort();
  });
  await page.goto(panel + "/diagnostics/");
  await page.getByRole("link", { name: "Аккаунт", exact: true }).click();
  const input = page.locator("#mcp-account-callback");
  const form = page.locator("#mcp-account-manual");
  const link = page.locator("#mcp-account-authorize");
  const message = page.locator("#mcp-account-message");
  const start = page.locator("#mcp-account-start");
  const finish = page.locator("#mcp-account-finish button[type=submit]");
  const select = page.locator("#mcp-account-server");
  const status = page.locator("#mcp-account-status");
  await select.selectOption("disabled");
  assert.ok(await start.isDisabled());
  await select.selectOption("catalog");

  async function consent() {
    await start.click();
    await form.waitFor();
    assert.equal(await input.getAttribute("type"), "password");
    assert.equal(await link.getAttribute("rel"), "noopener noreferrer");
    const [popup] = await Promise.all([context.waitForEvent("page", { timeout: 5000 }), link.click()]);
    popup.setDefaultTimeout(5000);
    await Promise.all([
      popup.waitForURL(url => url.origin + url.pathname === redirect),
      popup.getByRole("link", { name: "Подтвердить доступ" }).click(),
    ]);
    const callback = popup.url();
    await popup.close();
    return callback;
  }

  const first = await consent();
  await input.fill(first.replace("/callback?", "/wrong?"));
  const [mismatch] = await Promise.all([page.waitForResponse(api + "/complete"), finish.click()]);
  assert.equal(mismatch.status(), 400);
  await message.filter({ hasText: "Адрес возврата не соответствует" }).waitFor();
  assert.equal(await input.inputValue(), "");
  assert.ok(await form.isVisible());
  await input.fill(first);
  await finish.click();
  await status.filter({ hasText: "Подключён" }).waitFor();
  assert.equal(await input.inputValue(), "");
  assert.equal(await link.getAttribute("href"), null);

  const second = await consent();
  let submissions = 0;
  let signalSaved;
  const saved = new Promise(resolve => { signalSaved = resolve; });
  let release;
  const hold = new Promise(resolve => { release = resolve; });
  const holdCompletion = async route => {
    submissions++;
    try {
      const response = await route.fetch();
      signalSaved(response.status());
      await hold;
      await route.abort().catch(() => {});
    } catch {
      signalSaved(0);
    }
  };
  await page.route(api + "/complete", holdCompletion);
  try {
    await input.fill(second);
    await finish.click();
    assert.equal(await saved, 200);
    const reread = page.waitForResponse(api);
    await page.clock.fastForward(46000);
    assert.equal((await reread).status(), 200);
    await form.waitFor({ state: "hidden" });
    await message.filter({ hasText: "не отправляйте адрес повторно" }).waitFor();
    assert.equal(await input.inputValue(), "");
    assert.equal(await link.getAttribute("href"), null);
    assert.equal(submissions, 1);
    assert.ok(await status.filter({ hasText: "Подключён" }).isVisible());
    const persisted = await context.storageState();
    assert.ok(!JSON.stringify(persisted).includes(code));
    const session = await page.evaluate(() => Object.values(sessionStorage));
    assert.ok(!JSON.stringify(session).includes(code));
  } finally {
    release();
    await page.unroute(api + "/complete", holdCompletion);
  }

  const third = await consent();
  await input.fill(third);
  const completed = page.waitForResponse(api + "/complete");
  await Promise.all([page.waitForRequest(api + "/complete"), finish.click()]);
  assert.equal(await input.inputValue(), "");
  await page.waitForFunction(async () => Number(await (await fetch("/fixture-token-requests")).text()) === 3, null, { timeout: 5000 });
  const [canceled] = await Promise.all([
    page.waitForResponse(api + "/cancel"),
    page.locator("#mcp-account-cancel").click(),
  ]);
  assert.equal(canceled.status(), 200);
  assert.equal((await completed).status(), 409);
  await message.filter({ hasText: "Попытка входа отменена" }).waitFor();
  await form.waitFor({ state: "hidden" });
  assert.ok(await status.filter({ hasText: "Подключён" }).isVisible());
} finally {
  await browser.close();
}
