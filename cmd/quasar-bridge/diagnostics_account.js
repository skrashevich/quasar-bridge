"use strict";

let accountBusy = false;
let catalogVersion = 0;
let modelCatalog = null;

function syncModelControls() {
  const select = byId("account-model");
  select.disabled = accountBusy || !modelCatalog?.writable;
  byId("model-refresh").disabled = accountBusy;
  byId("model-save").disabled = select.disabled || !select.value ||
    (select.value === modelCatalog.active && select.value === modelCatalog.saved);
}

function clearModelCatalog(text = "") {
  catalogVersion++;
  modelCatalog = null;
  byId("account-model").replaceChildren(new Option(text, ""));
  syncModelControls();
}

async function loadAccountModels(refresh = false) {
  clearModelCatalog("Загружаем модели…");
  const version = catalogVersion;
  byId("model-refresh").disabled = true;
  message("model-message");
  try {
    const catalog = refresh ? await post("api/chatgpt/models", {}, 65000) : await request("api/chatgpt/models");
    if (version !== catalogVersion) return;
    const select = byId("account-model");
    if (!catalog.models.length) {
      select.replaceChildren(new Option("Список ещё не загружен", ""));
      message("model-message", "Нажмите «Обновить список», чтобы получить модели ChatGPT.");
      return;
    }
    modelCatalog = catalog;
    select.replaceChildren(...catalog.models.map(({ id, name }) => new Option(name === id ? id : name + " (" + id + ")", id)));
    if (catalog.models.some(model => model.id === catalog.saved)) select.value = catalog.saved;
    else {
      const prompt = new Option("Выберите доступную модель", "");
      prompt.disabled = true;
      select.prepend(prompt);
      select.value = "";
    }
    if (!catalog.writable) message("model-message", "Модель задана переменной QUASAR_LLM_MODEL в окружении службы.");
    else if (catalog.saved !== catalog.active) message("model-message", "В настройках сохранена модель " + catalog.saved + ". Сейчас действует " + catalog.active + ".");
  } catch (error) {
    if (version !== catalogVersion) return;
    byId("account-model").replaceChildren(new Option("Список недоступен", ""));
    message("model-message", error.message, true);
  } finally {
    if (version === catalogVersion) syncModelControls();
  }
}

async function applyAccountModel(event) {
  event.preventDefault();
  if (byId("model-save").disabled) return;
  const model = byId("account-model").value;
  accountControls(true);
  message("model-message", "Проверяем модель у провайдера…");
  try {
    const result = await post("api/chatgpt/model", { model, expected_model: modelCatalog.saved }, 65000);
    Object.assign(modelCatalog, result);
    message("model-message", "Для следующих запросов выбрана модель " + result.active + ".");
    await Promise.all([loadStatus(), loadConfig(false)]);
  } catch (error) {
    if (error.status === 409 || error.status === 422) clearModelCatalog("Обновите список моделей");
    message("model-message", error.message, true);
  } finally { accountControls(false); }
}

function accountControls(busy) {
  accountBusy = busy;
  for (const id of ["account-start", "account-refresh", "account-logout"]) byId(id).disabled = busy;
  byId("account-finish").querySelector("button").disabled = busy;
  syncModelControls();
}

async function accountStatus() {
  const account = await request("api/chatgpt");
  const signedIn = account.signed_in;
  fillFacts("account-state", [["Состояние входа", signedIn ? "Аккаунт подключён" : "Вход не выполнен"], ...(signedIn && account.email ? [["Аккаунт", account.email]] : [])]);
  byId("account-start").textContent = signedIn ? "Войти повторно" : "Войти через ChatGPT";
  byId("account-logout").hidden = !signedIn;
  const showModels = signedIn && account.uses_chatgpt;
  byId("account-model-form").hidden = !showModels;
  if (showModels) await loadAccountModels();
  else clearModelCatalog();
}

async function loadAccount() {
  if (accountBusy) return;
  accountControls(true);
  try { await accountStatus(); message("account-message"); }
  catch (error) {
    clearModelCatalog();
    byId("account-model-form").hidden = true;
    message("account-message", error.message, true);
  }
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
  byId("account-model").addEventListener("change", syncModelControls);
  byId("account-model-form").addEventListener("submit", applyAccountModel);
  byId("model-refresh").addEventListener("click", () => loadAccountModels(true));
  byId("account-start").addEventListener("click", startAccountLogin);
  byId("account-refresh").addEventListener("click", loadAccount);
  byId("account-finish").addEventListener("submit", finishAccountLogin);
  byId("account-logout").addEventListener("click", logoutAccount);
}
