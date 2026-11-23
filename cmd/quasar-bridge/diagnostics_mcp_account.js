"use strict";

const [setupMCPAccounts, loadMCPAccounts] = (() => {
  const api = "api/mcp-auth";
  const statusText = {
    "not signed in": "Вход не выполнен",
    "sign-in required": "Требуется вход",
    "access token valid": "Подключён",
    "refresh needed": "Доступ обновится при подключении",
    unavailable: "Хранилище недоступно",
  };
  const field = name => byId("mcp-account-" + name);
  const notify = (text, error) => message("mcp-account-message", text, error);
  let servers = [], active = null, busy = false, cancelBusy = false, expiry;
  const selected = () => servers.find(server => server.name === field("server").value);

  function render() {
    const server = selected();
    field("controls").hidden = !server;
    const locked = busy || cancelBusy;
    field("server").disabled = locked;
    field("status").textContent = server ? (server.disabled ? "Сервер отключён · " : "") + statusText[server.status] : "";
    field("start").disabled = locked || !server || server.disabled || server.status === "unavailable";
    field("logout").disabled = locked || !server || ["not signed in", "unavailable"].includes(server.status);
    field("refresh").disabled = locked;
    field("callback").disabled = locked;
    field("finish").querySelector("button").disabled = locked;
    field("attempt-actions").hidden = !active && !cancelBusy;
    field("cancel").disabled = cancelBusy;
  }

  function hideManual() {
    field("callback").value = "";
    field("authorize").removeAttribute("href");
    field("manual").hidden = true;
  }

  function clearAttempt() {
    window.clearTimeout(expiry);
    active = null;
    hideManual();
    render();
  }

  function showManual(link) {
    field("authorize").href = link;
    field("manual").hidden = false;
  }

  async function refresh() {
    const { servers: latest } = await request(api);
    const select = field("server"), previous = select.value;
    servers = latest;
    select.replaceChildren(...servers.map(server => new Option(server.name, server.name)));
    if (servers.some(server => server.name === previous)) select.value = previous;
    if (active && active.server !== select.value) clearAttempt();
    render();
  }

  async function refreshAndNotify(text, error = false, stillRelevant = () => true) {
    try { await refresh(); }
    catch (_) {
      if (stillRelevant()) notify(text + " Не удалось обновить статус MCP.", true);
      return;
    }
    if (stillRelevant()) notify(text, error);
  }

  async function run(action) {
    if (busy || cancelBusy) return;
    busy = true;
    render();
    try { await action(); }
    finally { busy = false; render(); }
  }

  function load() {
    return run(async () => {
      try {
        await refresh();
        if (!servers.length) notify("В конфигурации нет MCP-серверов с OAuth.");
        else if (!active) notify();
      } catch (_) { notify("Не удалось загрузить статус MCP. Откройте раздел заново.", true); }
    });
  }

  function start() {
    const server = selected();
    if (field("start").disabled || !server) return;
    return run(async () => {
      clearAttempt();
      notify("Подготавливаем вход в MCP…");
      try {
        const data = await post(api + "/start", { server: server.name }, 20000);
        active = { server: server.name, attempt_id: data.attempt_id };
        showManual(data.authorization_url);
        expiry = window.setTimeout(() => {
          clearAttempt();
          notify("Время входа истекло. Начните вход заново.", true);
        }, 600000);
        notify();
        if (!byId("account").hidden) field("authorize").focus();
      } catch (error) { notify(error.message, true); }
    });
  }

  function complete(event) {
    event.preventDefault();
    if (busy || cancelBusy || !active) return;
    const callback = field("callback").value.trim();
    field("callback").value = "";
    if (!callback) return;
    const attempt = active;
    const link = field("authorize").href;
    hideManual();
    return run(async () => {
      notify("Завершаем вход в MCP…");
      try {
        await post(api + "/complete", { ...attempt, callback }, 45000);
        if (active !== attempt) return;
        clearAttempt();
        await refreshAndNotify("Вход в MCP выполнен.");
      } catch (error) {
        if (active !== attempt) return;
        if (error.status === 400) {
          showManual(link);
          notify(error.message, true);
          return;
        }
        const uncertain = !error.status || error.status >= 500;
        if (!uncertain) clearAttempt();
        await refreshAndNotify(uncertain ? "Результат входа не подтверждён. Проверьте статус и не отправляйте адрес повторно." : error.message,
          true, () => !uncertain || active === attempt);
      }
    });
  }

  async function cancel() {
    const attempt = active;
    if (!attempt || cancelBusy) return;
    cancelBusy = true;
    clearAttempt();
    try {
      await post(api + "/cancel", attempt);
      await refreshAndNotify("Попытка входа отменена.");
    } catch (error) {
      await refreshAndNotify(error.message, true);
    } finally {
      cancelBusy = false;
      render();
    }
  }

  function logout() {
    const server = selected();
    if (!server || busy || !window.confirm("Удалить локальный доступ к этому MCP-серверу?")) return;
    return run(async () => {
      clearAttempt();
      try {
        await post(api + "/logout", { server: server.name });
        await refreshAndNotify("Локальный доступ удалён.");
      } catch (error) { await refreshAndNotify(error.message, true); }
    });
  }

  function setup() {
    field("server").addEventListener("change", () => {
      if (active) void post(api + "/cancel", active).catch(() => {});
      clearAttempt();
      notify();
      render();
    });
    field("refresh").addEventListener("click", load);
    field("start").addEventListener("click", start);
    field("finish").addEventListener("submit", complete);
    field("cancel").addEventListener("click", cancel);
    field("logout").addEventListener("click", logout);
    window.addEventListener("pagehide", clearAttempt);
  }

  return [setup, load];
})();
