// background.js — сервис-воркер моста уровня 4 (план «одним бинарем»).
//
// Логика короткого опроса: раз в ~1с GET /job у mcp-websearch на
// 127.0.0.1:8765. 204 → спим; 200 → исполняем задание и POSTим результат в
// /result/{id}. Сам поллинг — заодно MV3-keepalive; chrome.alarms каждые
// 30с — страховка на случай смерти воркера. Обрыв связи (мост не поднят /
// сменился владелец) → backoff 5с и снова: перезапускать ничего не надо.
//
// Дисциплина «косим под пользователя»: одна закреплённая видимая вкладка на
// все tab_read (сессия/куки переиспользуются), темп задаёт СЕРВЕР
// (min-interval 2с) — здесь никаких собственных очередей.

const DEFAULT_PORT = 8765;
const MAX_BODY = 5 * 1024 * 1024; // зеркалит maxBodyBytes сервера
const POLL_IDLE_MS = 1000; // пусто (204) → следующий опрос
const POLL_BACKOFF_MS = 5000; // мост недоступен / 401 / 5xx → пауза
const TAB_LOAD_TIMEOUT_MS = 20000;
const TAB_WAIT_MAX_MS = 15000;

let cfg = { port: DEFAULT_PORT, token: "" };
let loopToken = 0; // инкремент при (ре)старте убивает старый цикл — не бывает двух

const log = (...a) => console.log("[bridge]", ...a);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const bridgeURL = (path) => `http://127.0.0.1:${cfg.port}${path}`;

async function loadCfg() {
  const got = await chrome.storage.local.get(["port", "token"]);
  cfg = {
    port: Number(got.port) || DEFAULT_PORT,
    token: (got.token || "").trim(),
  };
}

async function postResult(id, payload) {
  try {
    await fetch(bridgeURL(`/result/${id}`), {
      method: "POST",
      headers: { "X-Bridge-Token": cfg.token, "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
  } catch (e) {
    // мост умер прямо во время ответа — задание всё равно завершено,
    // сервер отбросит результат по таймауту; молча живём дальше
    log("postResult failed:", e && e.message);
  }
}

// --- action: fetch — сетевой запрос ИЗ ПРОФИЛЯ браузера (куки, отпечаток) ---
// fetch() НЕ сниффает <meta charset> (в отличие от навигации) — чарсет
// берём из заголовка, иначе из первых байтов HTML, иначе utf-8.

function sniffCharset(buf, contentType) {
  const m = /charset=([\w-]+)/i.exec(contentType || "");
  if (m) return m[1];
  const head = new TextDecoder("utf-8", { fatal: false }).decode(buf.slice(0, 2048));
  const meta = /<meta[^>]+charset=["']?([\w-]+)/i.exec(head);
  return meta ? meta[1] : "utf-8";
}

async function doFetch(job) {
  try {
    const resp = await fetch(job.url, { credentials: "include", redirect: "follow" });
    let buf = await resp.arrayBuffer();
    let sliced = false;
    if (buf.byteLength > MAX_BODY) {
      buf = buf.slice(0, MAX_BODY);
      sliced = true;
    }
    let body;
    try {
      body = new TextDecoder(sniffCharset(buf, resp.headers.get("content-type") || "")).decode(buf);
    } catch {
      body = new TextDecoder("utf-8").decode(buf);
    }
    // ok=true означает «получен HTTP-ответ»: 403/498 — это тоже результат,
    // который вызывающий хочет увидеть; сетевые ошибки уходят в ok=false.
    await postResult(job.id, {
      ok: true,
      status: resp.status,
      final_url: resp.url,
      body,
      err: sliced ? "body capped at 5MB" : "",
    });
  } catch (e) {
    await postResult(job.id, { ok: false, err: String((e && e.message) || e) });
  }
}

// --- action: tab_read — видимая закреплённая вкладка, JS исполняется как у юзера ---

async function ensureTab() {
  const got = await chrome.storage.session.get(["tabId"]);
  if (got.tabId != null) {
    try {
      const t = await chrome.tabs.get(got.tabId);
      if (t) return t.id; // вкладка жива — переиспользуем (куки/сессия!)
    } catch {}
  }
  const tab = await chrome.tabs.create({ active: true, pinned: true, index: 0 });
  await chrome.storage.session.set({ tabId: tab.id });
  return tab.id;
}

// waitOrigin ждёт, пока вкладка ОКАЖЕТСЯ на ожидаемом origin и догрузится.
// Надёжнее события onUpdated: мгновенная проверка «status=complete» ловит
// СТАРУЮ страницу до старта навигации (гонка, из-за которой fetch уходил
// не с того origin и падал по CORS).
async function waitOrigin(tabId, originPrefix, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const t = await chrome.tabs.get(tabId);
      if ((!originPrefix || (t.url || "").startsWith(originPrefix)) && t.status === "complete") {
        return true;
      }
    } catch {
      return false; // вкладка умерла
    }
    await sleep(150);
  }
  return false; // таймаут — но страницу всё равно попробуем прочитать
}

function urlPrefix(u) {
  try {
    return new URL(u).origin + "/";
  } catch {
    return ""; // не URL (about:blank и т.п.) — ждать только complete
  }
}

async function doTabRead(job) {
  let tabId;
  try {
    tabId = await ensureTab();
    await chrome.tabs.update(tabId, { active: true, url: job.url });
    await waitOrigin(tabId, urlPrefix(job.url), TAB_LOAD_TIMEOUT_MS);
    const wait = Math.min(Math.max(job.wait_ms || 3000, 0), TAB_WAIT_MAX_MS);
    if (wait > 0) await sleep(wait); // даём JS/челленджам успокоиться
    // executeScript, а не evaluate: страница читается в её контексте;
    // синтетические события тут не шлём — только чтение
    const [res] = await chrome.scripting.executeScript({
      target: { tabId },
      func: () => {
        // Ссылки — первый класс результата: на SERP карточка товара часто
        // САМА <a> (WB, Ozon), её innerText = бренд+название+цена, а href
        // несёт id (nmID/SKU). Универсальнее любых API (им не страшны
        // смены версий), кап 300 + дедуп по href.
        const links = [];
        const seen = new Set();
        for (const a of document.querySelectorAll("a[href]")) {
          const href = a.href;
          if (!href || seen.has(href)) continue;
          seen.add(href);
          links.push({ href, text: (a.innerText || "").replace(/\s+/g, " ").trim().slice(0, 160) });
          if (links.length >= 300) break;
        }
        return {
          title: document.title,
          url: location.href,
          text: ((document.body && document.body.innerText) || "").slice(0, 512 * 1024),
          links,
        };
      },
    });
    await postResult(job.id, {
      ok: true,
      final_url: res.result.url,
      title: res.result.title,
      body: res.result.text,
      links: res.result.links,
    });
  } catch (e) {
    await postResult(job.id, { ok: false, err: String((e && e.message) || e) });
  }
}

// --- action: page_fetch — fetch В КОНТЕКСТЕ страницы Origin ---
// Для API за проверкой Origin/Referer (например search.wb.ru: SW-fetch
// без Origin → 403, XHR самой страницы проходит). Навигация в закреплённую
// вкладку на Origin, затем executeScript с async-func: её fetch несёт
// Origin страницы + куки — ровно как собственный XHR сайта.

async function doPageFetch(job) {
  let tabId;
  try {
    tabId = await ensureTab();
    await chrome.tabs.update(tabId, { active: true, url: job.origin || job.url });
    const prefix = urlPrefix(job.origin || job.url);
    await waitOrigin(tabId, prefix, TAB_LOAD_TIMEOUT_MS);
    const [res] = await chrome.scripting.executeScript({
      target: { tabId },
      // try/catch обязателен: отказ async-func проглатывается и результат
      // приходит null (именно так проявился CORS-отказ не с того origin)
      func: async (u) => {
        try {
          for (let i = 0; i < 100 && document.readyState !== "complete"; i++) {
            await new Promise((r) => setTimeout(r, 100));
          }
          const r = await fetch(u, { credentials: "include" });
          const t = await r.text();
          return { status: r.status, url: r.url, text: t.slice(0, 512 * 1024), err: "" };
        } catch (e) {
          return { status: 0, url: location.href, text: "", err: String((e && e.message) || e) };
        }
      },
      args: [job.url],
    });
    const v = res && res.result;
    if (!v) {
      await postResult(job.id, { ok: false, err: "injected fetch returned null" });
      return;
    }
    // ok=true = получили HTTP-ответ (как в doFetch); статус решает вызывающий
    await postResult(job.id, {
      ok: v.status > 0 && v.status < 500,
      status: v.status,
      final_url: v.url,
      body: v.text,
      err: v.status > 0 ? "" : `${v.err || "page fetch failed"} @ ${v.url}`,
    });
  } catch (e) {
    await postResult(job.id, { ok: false, err: String((e && e.message) || e) });
  }
}

// --- цикл опроса ---

async function pollLoop(myToken) {
  while (loopToken === myToken) {
    try {
      const resp = await fetch(bridgeURL("/job"), { headers: { "X-Bridge-Token": cfg.token } });
      if (resp.status === 204) {
        await sleep(POLL_IDLE_MS); // пусто — обычный холостой ход
      } else if (resp.status === 200) {
        const job = await resp.json();
        log(`job ${job.id} ${job.action} ${job.url}`);
        if (job.action === "fetch") await doFetch(job);
        else if (job.action === "tab_read") await doTabRead(job);
        else if (job.action === "page_fetch") await doPageFetch(job);
        else await postResult(job.id, { ok: false, err: "unknown action " + job.action });
        // после задания — СРАЗУ новый опрос: темп держит сервер (min-interval)
      } else if (resp.status === 401) {
        log("токен отклонён (401) — вставь правильный в настройках расширения");
        await sleep(POLL_BACKOFF_MS);
      } else {
        await sleep(POLL_BACKOFF_MS); // не наш ответ — подождём хозяина
      }
    } catch (e) {
      // connection refused: мост не поднят или владелец сменился —
      // перезапускать нечего, просто подольше спим и пробуем снова
      await sleep(POLL_BACKOFF_MS);
    }
  }
}

async function start() {
  await loadCfg();
  loopToken++;
  pollLoop(loopToken).catch((e) => log("loop crashed:", e));
}

chrome.runtime.onStartup.addListener(start);
chrome.runtime.onInstalled.addListener(start);
chrome.storage.onChanged.addListener((changes, area) => {
  // новые токен/порт в настройках → перезапуск цикла на лету
  if (area === "local" && (changes.port || changes.token)) start();
});
chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === "keepalive") start(); // страховка: цикл мёртв → поднимет новый
});
chrome.alarms.create("keepalive", { periodInMinutes: 0.5 });
start();
