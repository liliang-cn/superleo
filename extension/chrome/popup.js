const $ = (id) => document.getElementById(id);

async function refresh() {
  const s = await chrome.runtime.sendMessage({ type: "status" });
  const st = $("st");
  st.className = "st " + (s.connected ? "on" : s.error ? "err" : "");
  $("stt").textContent = s.connected ? "已连接" : s.error ? "未连接：" + s.error : s.code ? "连接中…" : "未连接";
  if (!$("code").value && s.code) $("code").value = s.code;
}

$("go").onclick = async () => {
  await chrome.runtime.sendMessage({ type: "connect", code: $("code").value.trim() });
  setTimeout(refresh, 600);
};
$("off").onclick = async () => {
  await chrome.runtime.sendMessage({ type: "disconnect" });
  refresh();
};
refresh();
setInterval(refresh, 1500);

$("panel").onclick = async () => {
  const w = await chrome.windows.getCurrent();
  await chrome.sidePanel.open({ windowId: w.id });
  window.close();
};
