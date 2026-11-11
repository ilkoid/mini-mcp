// options.js — настройка моста: токен + порт (chrome.storage.local).
const $ = (id) => document.getElementById(id);
const msg = (text, cls) => {
  const m = $("msg");
  m.textContent = text;
  m.className = cls; // ok | err
};

async function load() {
  const got = await chrome.storage.local.get(["token", "port"]);
  $("token").value = got.token || "";
  $("port").value = Number(got.port) || 8765;
}

$("save").onclick = async () => {
  const port = Number($("port").value) || 8765;
  await chrome.storage.local.set({ token: $("token").value.trim(), port });
  msg("Сохранено. Фоновый цикл перезапустится сам (storage.onChanged).", "ok");
};

$("test").onclick = async () => {
  msg("Проверяю…", "ok");
  const port = Number($("port").value) || 8765;
  const token = $("token").value.trim();
  try {
    const r = await fetch(`http://127.0.0.1:${port}/health`, {
      headers: { "X-Bridge-Token": token },
    });
    if (r.status === 200) msg("OK — мост отвечает, токен верный.", "ok");
    else if (r.status === 401) msg("HTTP 401 — мост жив, но токен неверный.", "err");
    else msg(`HTTP ${r.status} — это не наш мост (порт занят чужим?).`, "err");
  } catch (e) {
    msg("Мост не поднят. Запусти mcp-websearch (обычная регистрация в\n" +
        "агенте уже поднимает его лениво — вызови любой bridge-инструмент\n" +
        "или `mcp-websearch --selftest-bridge`).\n\n" + e, "err");
  }
};

load();
