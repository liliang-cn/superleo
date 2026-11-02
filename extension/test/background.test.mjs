// Runs the extension's service worker against a fake chrome.* and a fake
// WebSocket: `node --test extension/test`.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import path from "node:path";
import { fileURLToPath } from "node:url";

const src = fs.readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "../chrome/background.js"), "utf8");

function listeners() {
  const l = [];
  return { addListener: (f) => l.push(f), fire: (...a) => l.forEach((f) => f(...a)) };
}

// world builds a fresh service worker. tabs: [{id, groupId, url, title}], groups: [{id, title, windowId}].
async function world({ tabs, groups = [{ id: 10, title: "SuperLeo", windowId: 1 }] }) {
  const w = { sockets: [], sent: [], timers: [], debugCalls: [], removed: [], grouped: [], groupUpdates: [], created: [] };
  let nextTab = 100;
  let nextGroup = 50;
  w.tabs = tabs;
  w.chrome = null;
  w.groups = groups;
  const ev = () => listeners();
  const chrome = {
    runtime: { getManifest: () => ({ version: "9.9" }), onMessage: ev(), onStartup: ev(), onInstalled: ev() },
    storage: { local: { get: async () => ({ code: "43521:tok" }), set: async () => {}, remove: async () => {} } },
    alarms: { create() {}, onAlarm: ev() },
    tabs: {
      onRemoved: ev(),
      onUpdated: ev(),
      get: async (id) => {
        const t = w.tabs.find((x) => x.id === id);
        if (!t) throw new Error("No tab");
        return t;
      },
      query: async ({ groupId }) => w.tabs.filter((t) => t.groupId === groupId),
      create: async ({ windowId, url }) => {
        const t = { id: nextTab++, groupId: -1, windowId, url, title: "" };
        w.tabs.push(t);
        w.created.push(t);
        return t;
      },
      group: async ({ tabIds, groupId, createProperties }) => {
        let gid = groupId;
        if (gid === undefined) {
          gid = nextGroup++;
          w.groups.push({ id: gid, title: "", windowId: createProperties.windowId });
        }
        w.grouped.push({ tabIds, gid });
        for (const id of tabIds) w.tabs.find((t) => t.id === id).groupId = gid;
        return gid;
      },
      remove: async (id) => {
        w.removed.push(id);
        w.tabs = w.tabs.filter((t) => t.id !== id);
      },
      update: async (id) => w.tabs.find((t) => t.id === id),
    },
    tabGroups: {
      onUpdated: ev(),
      onRemoved: ev(),
      get: async (id) => {
        const g = w.groups.find((x) => x.id === id);
        if (!g) throw new Error("No group");
        return g;
      },
      query: async ({ title }) => w.groups.filter((g) => g.title === title),
      update: async (id, props) => {
        w.groupUpdates.push({ id, ...props });
        Object.assign(w.groups.find((g) => g.id === id), props);
      },
    },
    windows: { getLastFocused: async () => ({ id: 1 }), create: async () => ({ id: 2 }), update: async () => {} },
    debugger: {
      onEvent: ev(),
      onDetach: ev(),
      attach: async () => {},
      detach: async () => {},
      sendCommand: async (target, method, params) => {
        w.debugCalls.push({ target, method, params });
        return { ok: true };
      },
      getTargets: async () => w.tabs.map((t) => ({ id: `T${t.id}`, type: "page", tabId: t.id, url: t.url || "", title: t.title || "" })),
    },
  };
  w.chrome = chrome;
  class WS {
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      w.sockets.push(this);
      queueMicrotask(() => {
        this.readyState = 1;
        this.onopen?.();
      });
    }
    send(s) {
      w.sent.push(JSON.parse(s));
    }
    close() {
      this.readyState = 3;
      this.onclose?.();
    }
  }
  WS.OPEN = 1;
  WS.CONNECTING = 0;
  const ctx = vm.createContext({
    chrome,
    WebSocket: WS,
    navigator: { userAgent: "Mozilla/5.0 Chrome/142.0.0.0 Safari/537.36" },
    setTimeout: (f, ms) => w.timers.push({ f, ms }) && w.timers.length,
    clearTimeout() {},
    setInterval: () => 1,
    clearInterval() {},
    encodeURIComponent,
    console,
  });
  vm.runInContext(src, ctx);
  await tick();
  w.sock = w.sockets[0];
  // call sends a CDP command as SuperLeo would and returns the reply.
  let id = 0;
  w.call = async (method, params = {}, sessionId) => {
    const my = ++id;
    w.sock.onmessage({ data: JSON.stringify({ id: my, method, params, sessionId }) });
    for (let i = 0; i < 50; i++) {
      await tick();
      const r = w.sent.find((m) => m.id === my);
      if (r) return r;
    }
    throw new Error("no reply to " + method);
  };
  return w;
}

const tick = () => new Promise((r) => setTimeout(r, 2));

const mixed = () => [
  { id: 1, groupId: 10, url: "https://news.ycombinator.com/", title: "HN" },
  { id: 2, groupId: -1, url: "https://mail.example/inbox", title: "Mail" },
  { id: 3, groupId: 99, url: "https://bank.example/", title: "Bank" },
];
const groups = () => [
  { id: 10, title: "SuperLeo", windowId: 1 },
  { id: 99, title: "Work", windowId: 1 },
];

test("dials SuperLeo with the code and says hello", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  assert.equal(w.sock.url, "ws://127.0.0.1:43521/ext?token=tok");
  assert.equal(w.sent[0].type, "hello");
});

test("only tabs in the SuperLeo group are listed", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  const r = await w.call("Target.getTargets");
  assert.deepEqual(r.result.targetInfos.map((t) => t.url), ["https://news.ycombinator.com/"]);
});

test("a tab outside the group cannot be attached, read, closed or activated", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  for (const [m, p] of [
    ["Target.attachToTarget", { targetId: "T2", flatten: true }],
    ["Target.attachToTarget", { targetId: "T3", flatten: true }],
    ["Target.closeTarget", { targetId: "T2" }],
    ["Target.activateTarget", { targetId: "T3" }],
    ["Target.getTargetInfo", { targetId: "T2" }],
  ]) {
    const r = await w.call(m, p);
    assert.ok(r.error, `${m} ${p.targetId} should be refused`);
  }
  assert.deepEqual(w.removed, []);
  const s = await w.call("Runtime.evaluate", { expression: "document.cookie" }, "sl2");
  assert.ok(s.error);
  assert.equal(w.debugCalls.length, 0, "nothing reached the debugger");
});

test("a page session cannot close another target", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  const a = await w.call("Target.attachToTarget", { targetId: "T1", flatten: true });
  assert.equal(a.result.sessionId, "sl1");
  const r = await w.call("Target.closeTarget", { targetId: "T2" }, "sl1");
  assert.ok(r.error);
  assert.deepEqual(w.removed, []);
});

test("commands in a group tab reach the debugger", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  await w.call("Target.attachToTarget", { targetId: "T1", flatten: true });
  const r = await w.call("Page.navigate", { url: "https://example.org" }, "sl1");
  assert.deepEqual(r.result, { ok: true });
  assert.equal(JSON.stringify(w.debugCalls[0].target), JSON.stringify({ tabId: 1 }));
});

test("a tab dragged out of the group is cut off", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  await w.call("Target.attachToTarget", { targetId: "T1", flatten: true });
  w.tabs[0].groupId = -1;
  const r = await w.call("Page.reload", {}, "sl1");
  assert.ok(r.error);
  assert.equal((await w.call("Target.getTargets")).result.targetInfos.length, 0);
});

test("createTarget opens a tab in the group, making it when it is missing", async () => {
  const w = await world({ tabs: [{ id: 2, groupId: -1, url: "https://mail.example/", title: "Mail" }], groups: [] });
  const r = await w.call("Target.createTarget", { url: "about:blank" });
  assert.equal(r.result.targetId, "T100");
  assert.equal(w.created[0].active, undefined);
  assert.equal(w.grouped.length, 1);
  const g = w.groups.find((x) => x.id === w.grouped[0].gid);
  assert.equal(g.title, "SuperLeo");
  assert.equal(g.color, "blue");
  // Only the new tab is in it; the person's mail is not.
  assert.equal(JSON.stringify(w.grouped[0].tabIds), "[100]");
  const list = await w.call("Target.getTargets");
  assert.deepEqual(list.result.targetInfos.map((t) => t.targetId), ["T100"]);
  // The second tab joins the same group.
  await w.call("Target.createTarget", { url: "about:blank" });
  assert.equal(w.groups.filter((x) => x.title === "SuperLeo").length, 1);
  assert.equal(w.grouped[1].gid, w.grouped[0].gid);
});

test("two tabs opened at once share one new group", async () => {
  const w = await world({ tabs: [], groups: [] });
  await Promise.all([w.call("Target.createTarget", { url: "about:blank" }), w.call("Target.createTarget", { url: "about:blank" })]);
  assert.equal(w.groups.length, 1);
});

test("a tab of its own can be closed", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  const r = await w.call("Target.closeTarget", { targetId: "T1" });
  assert.deepEqual(r.result, { success: true });
  assert.deepEqual(w.removed, [1]);
});

test("closing Chrome and new browser contexts are refused", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  for (const m of ["Browser.close", "Browser.crash", "Target.createBrowserContext", "Network.clearBrowserCookies"]) {
    assert.ok((await w.call(m)).error, m);
  }
  assert.ok((await w.call("Browser.close", {}, "sl1")).error);
  assert.ok((await w.call("SystemInfo.getInfo")).error, "unknown browser-level commands are refused");
});

test("events of a tab are forwarded under its session", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  await w.call("Target.attachToTarget", { targetId: "T1", flatten: true });
  w.chrome.debugger.onEvent.fire({ tabId: 1 }, "Page.loadEventFired", { timestamp: 1 });
  w.chrome.debugger.onEvent.fire({ tabId: 2 }, "Page.loadEventFired", { timestamp: 2 });
  const evs = w.sent.filter((m) => m.method === "Page.loadEventFired");
  assert.deepEqual(evs.map((e) => e.sessionId), ["sl1"], "an unattached tab says nothing");
});

test("reconnects after the socket drops", async () => {
  const w = await world({ tabs: mixed(), groups: groups() });
  w.sock.close();
  assert.ok(w.timers.length > 0, "a retry is scheduled");
  w.timers.at(-1).f();
  await tick();
  assert.equal(w.sockets.length, 2);
  assert.equal(w.sockets[1].readyState, 1);
  w.sock = w.sockets[1];
  assert.equal((await w.call("Target.getTargets")).result.targetInfos.length, 1);
});
