"use strict";
(async () => {
  const callback = location.href;
  history.replaceState(null, "", location.pathname);
  try {
    const response = await fetch("/diagnostics/api/chatgpt/complete", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ callback }), signal: AbortSignal.timeout(45000)
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Не удалось подтвердить вход.");
    location.replace("/diagnostics/#account");
  } catch (error) {
    document.getElementById("result").textContent = error.message;
  }
})();
