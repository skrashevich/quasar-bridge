import assert from "node:assert/strict";

export async function declineAccountLogin(page) {
  // Only the provider consent screen is synthetic. Bridge handles its own callback.
  const url = page.url();
  await page.route("https://auth.openai.com/api/accounts/authorize?*", async route => {
    const params = new URL(route.request().url()).searchParams;
    const callback = new URL(params.get("redirect_uri"));
    assert.equal(callback.href, new URL("/auth/callback", url).href);
    assert.ok(params.get("state"));
    callback.search = new URLSearchParams({ state: params.get("state"), error: "access_denied" });
    await route.fulfill({ contentType: "text/html; charset=utf-8", body: '<a href="' + callback.href.replaceAll("&", "&amp;") + '">Отмена</a>' });
  });
  await page.getByRole("button", { name: "Войти через ChatGPT" }).click();
  await page.getByRole("link", { name: "Отмена" }).click();
  await page.locator("#result").filter({ hasText: "Вход отменён" }).waitFor();
  assert.equal(new URL(page.url()).search, "");
}
