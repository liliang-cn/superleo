import { ViewKey } from "./types";

// Where each screen lives.
//
// The app used to be one component holding "which screen" in state, which made
// every screen unreachable except by clicking to it: no link to send, no back
// button, and a reload landed on Chat whatever had been open. The address is now
// the state, and this is the table both directions read.
//
// Hash routing, not history routing. The same bundle runs inside the desktop
// window, where there is no server to answer an arbitrary path with the app, and
// in a browser behind one; a path after the # works identically in both.
export const PATHS: Record<ViewKey, string> = {
  home: "/",
  chat: "/chat",
  conversations: "/conversations",
  stats: "/stats",
  hive: "/hive",
  agents: "/agents",
  coding: "/coding",
  tasks: "/tasks",
  schedule: "/schedule",
  dashboards: "/dashboards",
  knowledge: "/knowledge",
  skills: "/skills",
  mcp: "/mcp",
  records: "/records",
  settings: "/settings",
};

/** Which screen an address belongs to. Anything unknown is home. */
export function viewOf(pathname: string): ViewKey {
  if (pathname === "/" || pathname === "") return "home";
  const first = "/" + pathname.split("/")[1];
  const hit = (Object.keys(PATHS) as ViewKey[]).find((k) => PATHS[k] === first);
  return hit ?? "home";
}

/** Where a notice's deeplink opens in this app, or "" for nowhere. */
export function deeplinkPath(link?: string): string {
  if (!link) return "";
  if (/^super(?:ai|leo):\/\/schedule\b/.test(link) || /^noted:\/\/(event|task|alarm)\//.test(link)) return PATHS.schedule;
  return "";
}

/** The address of one hive task. The id is a UUID, so it needs no escaping. */
export const taskPath = (id: string) => `/hive/tasks/${id}`;

/** Open the exact item that raised an attention card. */
export function attentionPath(item: { kind: string; open?: string; ref?: string }): string {
  if (item.kind === "failed" && item.ref) return taskPath(item.ref);
  if (item.open === "agents") return PATHS.agents + (item.ref ? `?agent=${encodeURIComponent(item.ref)}` : "");
  if (item.open === "coding") return PATHS.coding + (item.ref ? `?run=${encodeURIComponent(item.ref)}` : "");
  if (item.kind === "reminder") return PATHS.records + "?tab=reminders";
  if (item.open === "records") return PATHS.records;
  return PATHS.hive;
}
