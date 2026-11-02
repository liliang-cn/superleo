// SuperLeo's Chrome extension.
//
// SuperLeo dials in over a WebSocket on loopback and sends Chrome DevTools
// Protocol commands; this worker runs them with chrome.debugger. It acts as a
// CDP browser that has exactly one kind of target: the tabs in the tab group
// named "SuperLeo". Every other tab is invisible (Target.getTargets leaves it
// out) and untouchable (every command is checked against the live group
// membership of the tab it names). Tabs SuperLeo opens are put in that group.

const GROUP_TITLE = "SuperLeo";
const GROUP_COLOR = "blue";
const VERSION = chrome.runtime.getManifest().version;

// Refused anywhere, whoever asks.
const REFUSED = {
  "Browser.close": "closing Chrome is the person's to do",
  "Browser.crash": "not allowed",
  "Browser.crashGpuProcess": "not allowed",
  "Target.createBrowserContext": "tabs stay in the SuperLeo tab group",
  "Target.disposeBrowserContext": "tabs stay in the SuperLeo tab group",
  // These reach the whole profile, not a tab.
  "Network.clearBrowserCookies": "cookies are the person's",
  "Network.clearBrowserCache": "the cache is the person's",
  "Storage.clearCookies": "cookies are the person's",
};

let ws = null;
let connected = false;
let lastError = "";
let retryTimer = 0;
let retryMs = 1000;
let pingTimer = 0;
let wantClients = 1; // how many CDP clients SuperLeo reports (0: let go of the tabs)
let idleTimer = 0;

const attached = new Set(); // tabIds we hold the debugger on
const children = new Map(); // child sessionId -> tabId
let discover = false;
let autoAttach = false;
let groupLock = Promise.resolve();

// ---- connection ------------------------------------------------------------

// The code the person pasted, if it parses; otherwise the pair.json SuperLeo
// writes into this extension's folder, so pairing needs nothing from them.
async function loadCode() {
  const { code } = await chrome.storage.local.get("code");
  if (parseCode(code)) return code;
  try {
    const r = await fetch(chrome.runtime.getURL("pair.json"), { cache: "no-store" });
    if (r.ok) {
      const p = await r.json();
      if (p && p.port && p.token) return `${p.port}:${p.token}`;
    }
  } catch (_) {}
  return code || "";
}

function parseCode(code) {
  const m = /^(\d{2,5})[:.](.+)$/.exec((code || "").trim());
  return m ? { port: Number(m[1]), token: m[2].trim() } : null;
}

async function connect() {
  clearTimeout(retryTimer);
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
  const raw = await loadCode();
  const c = parseCode(raw);
  if (!c) {
    lastError = raw ? "连接码不对" : "";
    return;
  }
  let sock;
  try {
    sock = new WebSocket(`ws://127.0.0.1:${c.port}/ext?token=${encodeURIComponent(c.token)}`);
  } catch (e) {
    lastError = String(e.message || e);
    return schedule();
  }
  ws = sock;
  sock.onopen = () => {
    connected = true;
    lastError = "";
    retryMs = 1000;
    send({ type: "hello", version: VERSION, browser: navigator.userAgent.match(/Chrom(e|ium)\/[\d.]+/)?.[0] || "Chrome" });
    clearInterval(pingTimer);
    // Traffic every 20s keeps this worker alive (Chrome 116+).
    pingTimer = setInterval(() => send({ type: "ping" }), 20000);
    syncTabs().catch(() => {});
  };
  sock.onmessage = (ev) => {
    let m;
    try {
      m = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (m.type === "clients") return onClients(m.n);
    if (m.type === "pong") return;
    if (m.method && m.id !== undefined) handle(m);
  };
  sock.onclose = () => {
    if (ws !== sock) return;
    connected = false;
    ws = null;
    clearInterval(pingTimer);
    if (!lastError) lastError = "连接断开";
    schedule();
  };
  sock.onerror = () => {
    lastError = "连不上 SuperLeo";
  };
}

function schedule() {
  clearTimeout(retryTimer);
  retryTimer = setTimeout(connect, retryMs);
  retryMs = Math.min(retryMs * 2, 15000);
}

function disconnect() {
  clearTimeout(retryTimer);
  const s = ws;
  ws = null;
  connected = false;
  if (s) s.close();
  releaseAll();
}

function send(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// Nobody is driving: let go of the tabs, which takes the "debugging" bar off them.
function onClients(n) {
  wantClients = n;
  clearTimeout(idleTimer);
  if (n === 0) idleTimer = setTimeout(releaseAll, 15000);
}

function releaseAll() {
  for (const tabId of [...attached]) chrome.debugger.detach({ tabId }).catch(() => {});
  attached.clear();
  children.clear();
  discover = false;
  autoAttach = false;
}

chrome.runtime.onMessage.addListener((m, _s, reply) => {
  (async () => {
    if (m.type === "status") {
      return reply({ connected, error: lastError, code: await loadCode() });
    }
    if (m.type === "connect") {
      await chrome.storage.local.set({ code: m.code });
      lastError = parseCode(m.code) ? "" : "连接码不对";
      disconnect();
      retryMs = 1000;
      await connect();
      return reply({ ok: true });
    }
    if (m.type === "disconnect") {
      await chrome.storage.local.remove("code");
      lastError = "";
      disconnect();
      return reply({ ok: true });
    }
    reply({});
  })();
  return true;
});

// The worker is stopped when idle; an alarm wakes it and re-dials.
chrome.alarms.create("superleo-keepalive", { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener(() => connect());
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
connect();

// ---- the group ---------------------------------------------------------------

async function groupOf(tab) {
  if (!tab || tab.groupId === undefined || tab.groupId < 0) return null;
  try {
    const g = await chrome.tabGroups.get(tab.groupId);
    return g.title === GROUP_TITLE ? g : null;
  } catch {
    return null;
  }
}

async function ownedTab(tabId) {
  let tab;
  try {
    tab = await chrome.tabs.get(tabId);
  } catch {
    return null;
  }
  return (await groupOf(tab)) ? tab : null;
}

async function assertOwned(tabId) {
  const tab = await ownedTab(tabId);
  if (!tab) throw new Error("that tab is not in the SuperLeo tab group");
  return tab;
}

async function ownedTabs() {
  const groups = await chrome.tabGroups.query({ title: GROUP_TITLE });
  const out = [];
  for (const g of groups) out.push(...(await chrome.tabs.query({ groupId: g.id })));
  return out;
}

// One at a time, so two tabs opened together share one new group.
function inGroupLock(fn) {
  const run = groupLock.then(fn, fn);
  groupLock = run.catch(() => {});
  return run;
}

async function openInGroup(url) {
  return inGroupLock(async () => {
    const groups = await chrome.tabGroups.query({ title: GROUP_TITLE });
    let windowId;
    let groupId;
    if (groups.length) {
      groupId = groups[0].id;
      windowId = groups[0].windowId;
    } else {
      const w = await chrome.windows.getLastFocused({ windowTypes: ["normal"] }).catch(() => null);
      windowId = w?.id;
      if (windowId === undefined) windowId = (await chrome.windows.create({ focused: false })).id;
    }
    const tab = await chrome.tabs.create({ windowId, url: url || "about:blank", active: false });
    const gid = await chrome.tabs.group(groupId === undefined ? { tabIds: [tab.id], createProperties: { windowId } } : { tabIds: [tab.id], groupId });
    if (groupId === undefined) await chrome.tabGroups.update(gid, { title: GROUP_TITLE, color: GROUP_COLOR, collapsed: false });
    return tab;
  });
}

// ---- targets ---------------------------------------------------------------

async function debuggerTarget(tabId) {
  for (let i = 0; i < 20; i++) {
    const ts = await chrome.debugger.getTargets();
    const t = ts.find((x) => x.tabId === tabId && x.type === "page");
    if (t) return t;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error("Chrome has not made a target for the tab yet");
}

function infoOf(t, tabId) {
  return {
    targetId: t.id,
    type: "page",
    title: t.title || "",
    url: t.url || "",
    attached: attached.has(tabId),
    canAccessOpener: false,
    browserContextId: "SUPERLEO",
  };
}

async function targetInfos() {
  const tabs = await ownedTabs();
  const ids = new Set(tabs.map((t) => t.id));
  const ts = await chrome.debugger.getTargets();
  return ts.filter((t) => t.type === "page" && ids.has(t.tabId)).map((t) => infoOf(t, t.tabId));
}

async function tabOfTarget(targetId) {
  const ts = await chrome.debugger.getTargets();
  const t = ts.find((x) => x.id === targetId && x.tabId !== undefined);
  if (!t) throw new Error("no such target");
  await assertOwned(t.tabId);
  return t;
}

function sessionFor(tabId) {
  return `sl${tabId}`;
}

function sessionTab(sessionId) {
  const m = /^sl(\d+)$/.exec(sessionId);
  if (m) return { tabId: Number(m[1]), child: false };
  if (children.has(sessionId)) return { tabId: children.get(sessionId), child: true };
  return null;
}

async function attach(tabId) {
  if (attached.has(tabId)) return;
  try {
    await chrome.debugger.attach({ tabId }, "1.3");
  } catch (e) {
    // Attached by an earlier life of this worker is fine; by anything else is not.
    if (!/already attached/i.test(String(e.message))) throw e;
    try {
      await chrome.debugger.sendCommand({ tabId }, "Runtime.evaluate", { expression: "1" });
    } catch {
      throw new Error("another debugger (DevTools?) is attached to that tab");
    }
  }
  attached.add(tabId);
}

async function announce(tabId) {
  const t = await debuggerTarget(tabId);
  send({
    method: "Target.attachedToTarget",
    params: { sessionId: sessionFor(tabId), targetInfo: infoOf(t, tabId), waitingForDebugger: false },
  });
}

// ---- commands -------------------------------------------------------------

async function handle(m) {
  try {
    if (REFUSED[m.method]) throw new Error(`${m.method}: ${REFUSED[m.method]}`);
    const result = await dispatch(m);
    send({ id: m.id, result: result ?? {} });
  } catch (e) {
    send({ id: m.id, error: { code: -32000, message: String(e.message || e) } });
  }
}

const TARGET_METHODS = new Set([
  "Target.getTargets",
  "Target.getTargetInfo",
  "Target.createTarget",
  "Target.closeTarget",
  "Target.activateTarget",
  "Target.attachToTarget",
  "Target.detachFromTarget",
  "Target.setDiscoverTargets",
  "Target.getBrowserContexts",
]);

async function dispatch(m) {
  const { method, params = {}, sessionId } = m;
  // Target.* is answered here wherever it is asked: a page session must not
  // reach the other targets of the browser.
  if (TARGET_METHODS.has(method) || (!sessionId && (method.startsWith("Browser.") || method.startsWith("Target.")))) {
    return browserLevel(method, params);
  }
  if (!sessionId) throw new Error(`${method} is not available through SuperLeo`);
  const s = sessionTab(sessionId);
  if (!s) throw new Error(`no such session ${sessionId}`);
  await assertOwned(s.tabId);
  await attach(s.tabId);
  const target = s.child ? { tabId: s.tabId, sessionId } : { tabId: s.tabId };
  return chrome.debugger.sendCommand(target, method, params);
}

async function browserLevel(method, params) {
  switch (method) {
    case "Browser.getVersion":
      return {
        protocolVersion: "1.3",
        product: navigator.userAgent.match(/Chrom(e|ium)\/[\d.]+/)?.[0] || "Chrome",
        revision: "",
        userAgent: navigator.userAgent,
        jsVersion: "",
      };
    case "Target.getBrowserContexts":
      return { browserContextIds: [] };
    case "Target.setDiscoverTargets": {
      discover = !!params.discover;
      if (discover) for (const info of await targetInfos()) send({ method: "Target.targetCreated", params: { targetInfo: info } });
      return {};
    }
    case "Target.setAutoAttach": {
      autoAttach = !!params.autoAttach;
      if (autoAttach) {
        for (const tab of await ownedTabs()) {
          await attach(tab.id);
          await announce(tab.id);
        }
      }
      return {};
    }
    case "Target.getTargets":
      return { targetInfos: await targetInfos() };
    case "Target.getTargetInfo": {
      if (!params.targetId) {
        return { targetInfo: { targetId: "superleo-browser", type: "browser", title: "", url: "", attached: true, canAccessOpener: false } };
      }
      const t = await tabOfTarget(params.targetId);
      return { targetInfo: infoOf(t, t.tabId) };
    }
    case "Target.createTarget": {
      const tab = await openInGroup(params.url);
      const t = await debuggerTarget(tab.id);
      if (discover) send({ method: "Target.targetCreated", params: { targetInfo: infoOf(t, tab.id) } });
      if (autoAttach) {
        await attach(tab.id);
        await announce(tab.id);
      }
      return { targetId: t.id };
    }
    case "Target.closeTarget": {
      const t = await tabOfTarget(params.targetId);
      await chrome.tabs.remove(t.tabId);
      return { success: true };
    }
    case "Target.activateTarget": {
      const t = await tabOfTarget(params.targetId);
      const tab = await chrome.tabs.update(t.tabId, { active: true });
      await chrome.windows.update(tab.windowId, { focused: true }).catch(() => {});
      return {};
    }
    case "Target.attachToTarget": {
      const t = await tabOfTarget(params.targetId);
      await attach(t.tabId);
      return { sessionId: sessionFor(t.tabId) };
    }
    case "Target.detachFromTarget": {
      const s = sessionTab(params.sessionId || "");
      if (s && !s.child) {
        await chrome.debugger.detach({ tabId: s.tabId }).catch(() => {});
        attached.delete(s.tabId);
      }
      return {};
    }
    default:
      throw new Error(`${method} is not available through SuperLeo`);
  }
}

// ---- events from Chrome ------------------------------------------------------

chrome.debugger.onEvent.addListener((source, method, params) => {
  if (source.tabId === undefined || !attached.has(source.tabId)) return;
  const sessionId = source.sessionId || sessionFor(source.tabId);
  if (method === "Target.attachedToTarget" && params?.sessionId) children.set(params.sessionId, source.tabId);
  if (method === "Target.detachedFromTarget" && params?.sessionId) children.delete(params.sessionId);
  send({ method, params, sessionId });
});

chrome.debugger.onDetach.addListener((source, reason) => {
  const tabId = source.tabId;
  if (tabId === undefined || !attached.delete(tabId)) return;
  for (const [sid, t] of children) if (t === tabId) children.delete(sid);
  send({ method: "Target.detachedFromTarget", params: { sessionId: sessionFor(tabId), reason } });
});

// What the tab group holds, as last told: tabId -> targetId.
const known = new Map();

async function syncTabs() {
  const tabs = await ownedTabs();
  const ids = new Set(tabs.map((t) => t.id));
  for (const [tabId, targetId] of known) {
    if (ids.has(tabId)) continue;
    known.delete(tabId);
    if (attached.delete(tabId)) chrome.debugger.detach({ tabId }).catch(() => {});
    if (discover) send({ method: "Target.targetDestroyed", params: { targetId } });
  }
  for (const tab of tabs) {
    if (known.has(tab.id)) continue;
    let t;
    try {
      t = await debuggerTarget(tab.id);
    } catch {
      continue;
    }
    known.set(tab.id, t.id);
    if (discover) send({ method: "Target.targetCreated", params: { targetInfo: infoOf(t, tab.id) } });
    if (autoAttach) {
      await attach(tab.id).catch(() => {});
      await announce(tab.id).catch(() => {});
    }
  }
}

function resync() {
  if (connected) syncTabs().catch(() => {});
}

chrome.tabs.onRemoved.addListener((tabId) => {
  attached.delete(tabId);
  resync();
});
chrome.tabs.onUpdated.addListener((tabId, change) => {
  if (change.groupId !== undefined) resync();
  if ((change.url || change.title) && discover && known.has(tabId)) {
    debuggerTarget(tabId)
      .then((t) => send({ method: "Target.targetInfoChanged", params: { targetInfo: infoOf(t, tabId) } }))
      .catch(() => {});
  }
});
chrome.tabGroups.onUpdated.addListener(resync);
chrome.tabGroups.onRemoved.addListener(resync);
