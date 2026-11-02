const $ = (id) => document.getElementById(id);
let base = "";
let token = "";
let pending = ""; // request id being answered
let baseline = 0; // turns before the message was sent
let timer = 0;

async function pairing() {
  const { code } = await chrome.storage.local.get("code");
  const m = /^(\d{2,5})[:.](.+)$/.exec((code || "").trim());
  if (!m) return false;
  base = `http://127.0.0.1:${m[1]}`;
  token = m[2].trim();
  return true;
}

async function api(path, body) {
  const r = await fetch(base + path, {
    method: body ? "POST" : "GET",
    headers: { Authorization: `Bearer ${token}`, ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!r.ok) throw new Error(r.status === 403 ? "连接码不对" : `出错 ${r.status}`);
  return r.json();
}

// ---- tiny markdown -------------------------------------------------------------

const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

function inline(s) {
  return esc(s)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
    .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
}

function md(src) {
  const out = [];
  const lines = src.replace(/\r/g, "").split("\n");
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i];
    if (l.startsWith("```")) {
      const code = [];
      for (i++; i < lines.length && !lines[i].startsWith("```"); i++) code.push(lines[i]);
      out.push(`<pre><code>${esc(code.join("\n"))}</code></pre>`);
    } else if (/^\s*[-*] /.test(l)) {
      const items = [];
      for (; i < lines.length && /^\s*[-*] /.test(lines[i]); i++) items.push(`<li>${inline(lines[i].replace(/^\s*[-*] /, ""))}</li>`);
      i--;
      out.push(`<ul>${items.join("")}</ul>`);
    } else if (l.trim()) {
      const para = [];
      for (; i < lines.length && lines[i].trim() && !lines[i].startsWith("```") && !/^\s*[-*] /.test(lines[i]); i++) para.push(inline(lines[i]));
      i--;
      out.push(`<p>${para.join("<br>")}</p>`);
    }
  }
  return out.join("");
}

// ---- view ----------------------------------------------------------------------

function render(turns, running) {
  const log = $("log");
  const atEnd = log.scrollTop + log.clientHeight >= log.scrollHeight - 40;
  log.textContent = "";
  for (const t of turns) {
    if (t.kind || !t.content || (t.role !== "user" && t.role !== "assistant")) continue;
    const d = document.createElement("div");
    d.className = `m ${t.role}`;
    if (t.role === "user") d.textContent = t.content;
    else d.innerHTML = md(t.content);
    log.append(d);
  }
  if (running) {
    const d = document.createElement("div");
    d.className = "think";
    d.textContent = "思考中…";
    log.append(d);
  }
  if (atEnd || running) log.scrollTop = log.scrollHeight;
}

function status(text, bad) {
  $("st").textContent = text;
  $("st").className = bad ? "err" : "";
}

async function poll() {
  clearTimeout(timer);
  if (!(await pairing())) return status("未连接", true);
  try {
    const s = await api(`/chat/state${pending ? `?requestId=${encodeURIComponent(pending)}` : ""}`);
    const turns = (s.turns || []).filter((t) => !t.kind);
    const answered = turns.slice(baseline + 1).some((t) => t.role === "assistant" && t.content);
    const running = !!pending && (s.running || !answered);
    render(turns, running);
    status("已连接");
    $("send").disabled = running;
    if (!running) pending = "";
    if (running) timer = setTimeout(poll, 800);
  } catch (e) {
    status(String(e.message || e), true);
    pending = "";
    $("send").disabled = false;
  }
}

async function pageContext() {
  const [tab] = await chrome.tabs.query({ active: true, lastFocusedWindow: true });
  if (!tab || !/^https?:/.test(tab.url || "")) return "";
  let sel = "";
  try {
    const [r] = await chrome.scripting.executeScript({ target: { tabId: tab.id }, func: () => String(getSelection() || "") });
    sel = (r?.result || "").trim().slice(0, 4000);
  } catch {}
  return `[当前页面]\n标题：${tab.title || ""}\n网址：${tab.url}${sel ? `\n选中文字：${sel}` : ""}\n\n`;
}

async function send() {
  const text = $("msg").value.trim();
  if (!text || $("send").disabled) return;
  $("send").disabled = true;
  try {
    const before = await api("/chat/state");
    baseline = (before.turns || []).filter((t) => !t.kind).length;
    const ctx = $("attach").checked ? await pageContext() : "";
    $("msg").value = "";
    const r = await api("/chat/send", { message: ctx + text });
    pending = r.requestId || "x";
    poll();
  } catch (e) {
    status(String(e.message || e), true);
    $("send").disabled = false;
  }
}

$("attach").onchange = async () => {
  if (!$("attach").checked) return chrome.storage.local.set({ attach: false });
  // Reading the page's selection needs the site permission, asked once.
  const ok = await chrome.permissions.request({ origins: ["<all_urls>"] }).catch(() => false);
  $("attach").checked = ok;
  chrome.storage.local.set({ attach: ok });
};
$("send").onclick = send;
$("msg").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    send();
  }
});
chrome.storage.local.get("attach").then(({ attach }) => ($("attach").checked = !!attach));
poll();
