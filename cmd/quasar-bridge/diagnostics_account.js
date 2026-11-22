"use strict";

let accountBusy = false;

function accountControls(busy) {
  accountBusy = busy;
  for (const id of ["account-start", "account-refresh", "account-logout"]) byId(id).disabled = busy;
  byId("account-finish").querySelector("button").disabled = busy;
}

async function accountStatus() {
  const account = await request("api/chatgpt");
  const signedIn = account.signed_in;
  fillFacts("account-state", [["Состояние входа", signedIn ? "Аккаунт подключён" : "Вход не выполнен"], ...(signedIn && account.email ? [["Аккаунт", account.email]] : [])]);
  byId("account-start").textContent = signedIn ? "Войти повторно" : "Войти через ChatGPT";
  byId("account-logout").hidden = !signedIn;
}

async function loadAccount() {
  if (accountBusy) return;
  accountControls(true);
  try { await accountStatus(); message("account-message"); }
  catch (error) { message("account-message", error.message, true); }
  finally { accountControls(false); }
}

function closeManualLogin() {
  byId("account-manual").hidden = true;
  byId("account-authorize").removeAttribute("href");
  byId("account-callback").value = "";
}

async function startAccountLogin() {
  if (accountBusy) return;
  accountControls(true);
  closeManualLogin();
  message("account-message", "Подготавливаем вход…");
  try {
    const result = await post("api/chatgpt/start", {});
    if (result.automatic_return) {
      location.assign(result.authorization_url);
    } else {
      byId("account-authorize").href = result.authorization_url;
      byId("account-manual").hidden = false;
      message("account-message");
      byId("account-authorize").focus();
    }
  } catch (error) { message("account-message", error.message, true); }
  finally { accountControls(false); }
}

async function finishAccountLogin(event) {
  event.preventDefault();
  if (accountBusy) return;
  accountControls(true);
  message("account-message", "Подтверждаем вход…");
  try {
    const callback = byId("account-callback").value.trim();
    byId("account-callback").value = "";
    await post("api/chatgpt/complete", { callback }, 45000);
    closeManualLogin();
    await accountStatus();
    message("account-message", "Вход в ChatGPT выполнен.");
  } catch (error) {
    if (error.status === 422) closeManualLogin();
    message("account-message", error.message, true);
  }
  finally { accountControls(false); }
}

async function logoutAccount() {
  if (accountBusy || !window.confirm("Выйти из ChatGPT на этом Bridge? Для следующих запросов по подписке потребуется войти снова.")) return;
  accountControls(true);
  closeManualLogin();
  try {
    await post("api/chatgpt/logout", {}, 45000);
    message("account-message", "Вы вышли из ChatGPT.");
  } catch (error) { message("account-message", error.message, true); }
  finally {
    try { await accountStatus(); } catch (error) { message("account-message", error.message, true); }
    accountControls(false);
  }
}

function setupAccount() {
  byId("account-start").addEventListener("click", startAccountLogin);
  byId("account-refresh").addEventListener("click", loadAccount);
  byId("account-finish").addEventListener("submit", finishAccountLogin);
  byId("account-logout").addEventListener("click", logoutAccount);
}
