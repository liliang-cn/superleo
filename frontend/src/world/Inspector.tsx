import { useEffect, useState } from "react";
import { ArrowRightIcon, ChevronDownIcon, ChevronRightIcon, XIcon, Maximize2Icon, PlayIcon, PauseIcon, Trash2Icon, SquareIcon, SendIcon, RefreshCwIcon, SearchIcon, ZapIcon } from "lucide-react";
import {
  CLIRunDetail, InstallMCPServer, InstallSkill, SearchMCPServers, SearchSkills,
  CancelCLIRun, DeleteDashboard, DeleteStandingAgent, ForgetLife, HiveTaskDetail, MemoryRecall, PauseStandingAgent,
  RefreshDashboard, RemoveMCPServer, RemoveSkill, ResolveToolApproval, ResumeStandingAgent, SendChat, StandingReports,
  StartCLIRun, WakeStandingAgent,
} from "../../wailsjs/go/app/App";
import { type CCActivity, type CLIActivity, type HAService, type Host, type HostAgent } from "../canvas/data";
import { useI18n } from "../lib/i18n";
import { queenNamed, queenRef, useQueenName } from "../lib/queenName";
import { toast } from "../lib/toasts";
import { PATHS, attentionPath, taskPath } from "../lib/routes";
import { focusInWorld } from "./bus";
import Talk from "./Talk";
import CLISessions from "./CLISessions";
import WorkerKitCard from "./WorkerKit";
import MachineHost from "./MachineHost";
import type { Picked } from "./engine";
import { clisOf, sessionLine, type WorldData } from "./cluster";
import { agendaWhen, type AgendaEntry } from "./agenda";
import { cliName, haRoles, isCore, isHost, isStandby, kindLabel, unlinkedStatus } from "./hosts";

const KIND: Record<AgendaEntry["kind"], string> = { event: "Event", reminder: "Reminder", todo: "To-do", timer: "Timing" };

const HOST_STATE: Record<string, string> = { live: "Live", lost: "Not answering", configured: "Only in the settings" };
const SVC_STATE: Record<string, string> = { active: "Running", degraded: "Running, no standby ready", down: "Down" };

const fmt = (n: number) => (n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(Math.round(n)));
const when = (s?: string) => {
  const d = s ? new Date(s) : null;
  return d && !isNaN(+d) ? d.toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit", hour12: false }) : "";
};

/**
 * What is selected in the world, and everything that can be done with it.
 * Each kind of thing brings its own actions: a bee is woken, paused or told
 * something; a machine starts a coding run; an approval is answered here.
 * `open(path, panel)` goes to a page, optionally with its list showing.
 */
export default function Inspector({ picked, data, onClose, open, openDash, full, setFull }: {
  picked: Picked; data: WorldData; onClose: () => void; open: (path: string, panel?: boolean) => void;
  full: boolean; setFull: (full: boolean) => void;
  /** Shows a dashboard full size over the world. */
  openDash?: (id: string) => void;
}) {
  const { t } = useI18n();
  const queen = useQueenName();
  const [busy, setBusy] = useState("");
  const [text, setText] = useState("");
  const [confirm, setConfirm] = useState("");
  const [extra, setExtra] = useState<string>("");
  useEffect(() => { setText(""); setConfirm(""); setExtra(""); }, [picked.kind, picked.id]);

  /** Runs an action, says how it went, and keeps the button busy meanwhile. */
  const act = async (key: string, fn: () => Promise<unknown>, ok: string) => {
    setBusy(key);
    try { await fn(); toast.success(t(ok)); return true; }
    catch (e) { toast.error(String(e instanceof Error ? e.message : e)); return false; }
    finally { setBusy(""); }
  };
  /** For the bindings that answer "ok" or what went wrong. */
  const must = (r: unknown) => { if (typeof r === "string" && r !== "ok") throw new Error(r); };
  /** A destructive action asks once more, on the same button. */
  const twice = (key: string, fn: () => void) => () => { if (confirm === key) { setConfirm(""); fn(); } else setConfirm(key); };

  // The facts under the title fold to one line, and stay the way they were
  // left on every card.
  const [facts, setFactsState] = useState(() => { try { return localStorage.getItem("superleo.inspector.facts") !== "folded"; } catch { return true; } });
  const setFacts = (v: boolean) => { setFactsState(v); try { localStorage.setItem("superleo.inspector.facts", v ? "open" : "folded"); } catch { /* ignore */ } };
  const rows: [string, string][] = [];
  let kind = picked.kind;
  let body: React.ReactNode = null;
  const say = (placeholder: string, send: (s: string) => Promise<unknown>, ok: string) => (
    <form className="wi-say" onSubmit={(e) => { e.preventDefault(); const s = text.trim(); if (!s) return; void act("say", () => send(s), ok).then(() => setText("")); }}>
      <input value={text} onChange={(e) => setText(e.target.value)} placeholder={t(placeholder)} />
      <button type="submit" disabled={!text.trim() || busy === "say"} title={t("Send")}><SendIcon size={14} /></button>
    </form>
  );

  switch (picked.kind) {
    case "hall": {
      const tt = data.meter.total;
      kind = queenNamed(queen) ? queen.name : "Queen";
      rows.push(["State", data.busy || tt.live ? "Working" : "Idle"], ["Hive tokens", fmt(tt.tokens)], ["Tool calls", String(tt.calls)],
        ["Orders out", String(data.hive.tasks.filter((x) => x.state === "running").length)]);
      body = <Talk who={{ kind: "queen" }} placeholder={t("Talk to {name}…", { name: queenRef(queen) })} />;
      break;
    }
    case "worker": {
      const m = data.hive.members.find((x) => x.name === picked.id);
      const p = data.meter.members.find((x) => x.name === picked.id);
      const mine = data.hive.tasks.filter((x) => x.worker === picked.id);
      const now = mine.find((x) => x.state === "running");
      kind = "Worker";
      rows.push(["State", m?.state === "lost" ? "Not answering" : now ? "Working" : "Idle"]);
      const on = data.hosts?.find((h) => h.agents.some((a) => a.kind === "superleo" && a.name === picked.id));
      if (on) rows.push([kindLabel(on.kind), on.parent ? `${on.name} · ${data.hosts?.find((h) => h.id === on.parent)?.name ?? on.parent}` : on.name]);
      if (now) rows.push(["Doing", now.prompt]);
      if (p) rows.push(["Tokens", fmt(p.tokens)], ["Tool calls", String(p.calls)]);
      body = (
        <>
          <Talk who={{ kind: "worker", name: picked.id }} placeholder={t("Talk to {name}…", { name: picked.id.replace(/^superleo-/, "") })} />
          {mine.length > 0 && (
            <ul className="wi-list">
              {mine.slice(0, 4).map((x) => (
                <li key={x.id} onClick={() => open(taskPath(x.id))}><i className={`wi-dot ${x.state}`} />{x.prompt}<em>{t(x.state)}</em></li>
              ))}
            </ul>
          )}
          {m?.state !== "lost" && <WorkerKitCard worker={picked.id} />}
        </>
      );
      break;
    }
    case "order": {
      const x = data.hive.tasks.find((y) => y.id === picked.id);
      kind = "Order";
      if (x) rows.push(["To", x.worker], ["State", x.state], ["Tool", x.tool ?? "—"], ["Tool calls", String(x.tools ?? 0)]);
      body = <button className="wl-go" onClick={() => open(taskPath(picked.id))}>{t("Open full log")}<ArrowRightIcon size={14} /></button>;
      break;
    }
    case "bee": {
      const b = data.bees.find((x) => x.id === picked.id);
      kind = "Bee";
      if (b) {
        rows.push(["Watches", b.goal], ["State", b.waitingFor ? "Needs you" : b.running ? "Out now" : b.paused ? "Paused" : "Resting"]);
        if (b.nextDue) rows.push(["Next", when(b.nextDue)]);
        if (b.waitingFor) rows.push(["Asks", b.waitingFor]);
      }
      body = b && (
        <>
          <Latest id={b.id} />
          <Talk who={{ kind: "bee", id: b.id }} placeholder={b.waitingFor ? "Answer it…" : "Tell it something — it wakes to read it"} />
          <div className="wi-acts">
            <button disabled={!!b.running || busy === "wake"} onClick={() => act("wake", () => WakeStandingAgent(b.id, ""), "Woken")}><ZapIcon size={14} />{t("Wake now")}</button>
            {b.paused
              ? <button disabled={busy === "pause"} onClick={() => act("pause", () => ResumeStandingAgent(b.id), "Resumed")}><PlayIcon size={14} />{t("Resume")}</button>
              : <button disabled={busy === "pause"} onClick={() => act("pause", () => PauseStandingAgent(b.id), "Paused")}><PauseIcon size={14} />{t("Pause")}</button>}
            <button onClick={() => open(PATHS.agents + "?agent=" + encodeURIComponent(b.id), true)}>{t("Edit")}</button>
            <button className="danger" onClick={twice("del", () => act("del", () => DeleteStandingAgent(b.id), "Deleted").then(onClose))}>
              <Trash2Icon size={14} />{t(confirm === "del" ? "Delete it?" : "Delete")}
            </button>
          </div>
        </>
      );
      break;
    }
    case "machine": {
      const a = data.linked.find((x) => x.name === picked.id);
      // Core's own CLIs are not in the linked list; a linked machine says.
      const clis = a ? a.clis ?? [] : ["claude", "codex"];
      const host = picked.id;
      const mine = data.runs.filter((r) => (r.remote || "core") === host);
      const live = mine.filter((r) => r.state === "running");
      kind = "Machine";
      if (a) rows.push(["System", `${a.os} ${a.arch}`]);
      rows.push(["Running", String(live.length)], ["Sessions", String(mine.length)]);
      const term = a ? clisOf(a) : {};
      const tw = Object.values(term).reduce((n, x) => n + (x.working ?? 0), 0), tq = Object.values(term).reduce((n, x) => n + (x.waiting ?? 0), 0);
      if (Object.keys(term).length) rows.push(["Terminal sessions", `${tw} ${t("working")} · ${tq} ${t("waiting")}`]);
      body = (
        <>
          <TermSessions clis={term} />
          <MachineHost name={host} />
          {clis.length > 0 && <NewRun clis={clis} host={host} onStart={(agent, prompt) => act("run", () => StartCLIRun(agent, prompt, "", "", true), "Run started")} busy={busy === "run"} />}
          <ul className="wi-list">
            {mine.slice(0, 5).map((r) => <RunLine key={r.id} run={r} onStop={() => act("stop" + r.id, () => CancelCLIRun(r.id), "Stopped")} />)}
          </ul>
          {(clis.length > 0 || mine.length > 0) && <button className="wl-go ghost" onClick={() => open(PATHS.coding, true)}>{t("All coding runs")}<ArrowRightIcon size={14} /></button>}
        </>
      );
      break;
    }
    case "cli": case "agent": {
      // A coding CLI or a named agent on a linked machine: talk to it.
      const [name, ...rest] = picked.id.split(".");
      const host = rest.join(".") || "core";
      const mine = data.runs.filter((r) => (r.remote || "core") === host && (r.agent === picked.id || r.agent === name));
      const live = mine.filter((r) => r.state === "running");
      kind = picked.kind === "cli" ? "Coding agent" : "Agent";
      rows.push(["On", host], ["Running", String(live.length)]);
      // Its own terminal sessions, on a machine or on a relay.
      const on = data.linked.find((x) => x.name === host) ?? data.hive.relays?.find((x) => x.name === host);
      const mineTerm = on ? clisOf(on)[name] : undefined;
      body = (
        <>
          <TermSessions clis={mineTerm ? { [name]: mineTerm } : {}} />
          {picked.kind === "cli"
            ? <CLISessions agent={picked.id} placeholder={t("What should {name} do?", { name })} />
            : <Talk who={{ kind: "agent", name }} placeholder={t("Talk to {name}…", { name })} />}
          {live.length > 0 && (
            <ul className="wi-list">
              {live.map((r) => (
                <li key={r.id}><i className="wi-dot running" />{r.prompt}
                  <button className="wi-x" title={t("Stop")} onClick={() => act("stop" + r.id, () => CancelCLIRun(r.id), "Stopped")}><SquareIcon size={11} /></button>
                </li>
              ))}
            </ul>
          )}
        </>
      );
      break;
    }
    case "brain": {
      kind = "Shared brain";
      rows.push(["Store", "CortexDB, shared by every worker"]);
      body = (
        <>
          <form className="wi-say" onSubmit={(e) => { e.preventDefault(); const q = text.trim(); if (!q) return; setBusy("recall"); MemoryRecall(q).then(setExtra).catch((er) => setExtra(String(er))).finally(() => setBusy("")); }}>
            <input value={text} onChange={(e) => setText(e.target.value)} placeholder={t("Search what the hive knows…")} />
            <button type="submit" disabled={!text.trim() || busy === "recall"}><SearchIcon size={14} /></button>
          </form>
          {busy === "recall" && <p className="wi-note">{t("Searching…")}</p>}
          {extra && <pre className="wi-text">{extra}</pre>}
          <button className="wl-go ghost" onClick={() => open(PATHS.knowledge, true)}>{t("Open the graph")}<ArrowRightIcon size={14} /></button>
        </>
      );
      break;
    }
    case "dashboard": {
      kind = "Dashboard";
      const d = data.dashboards.find((x) => x.id === picked.id);
      if (d?.refreshed_at) rows.push(["Updated", when(d.refreshed_at)]);
      body = (
        <div className="wi-acts">
          <button onClick={() => (openDash ? openDash(picked.id) : open(PATHS.dashboards, true))}>{t("Open")}</button>
          <button disabled={busy === "refresh"} onClick={() => act("refresh", () => RefreshDashboard(picked.id), "Refreshing")}><RefreshCwIcon size={14} />{t("Refresh")}</button>
          <button className="danger" onClick={twice("del", () => act("del", () => DeleteDashboard(picked.id), "Deleted").then(onClose))}><Trash2Icon size={14} />{t(confirm === "del" ? "Delete it?" : "Delete")}</button>
        </div>
      );
      break;
    }
    case "skills":
      kind = "Skills";
      rows.push(["Installed", String(data.skills.length)]);
      body = <SkillShop installed={data.skills} />;
      break;
    case "mcphub":
      kind = "MCP servers";
      rows.push(["Connected", `${data.mcp.filter((m) => m.ok).length} of ${data.mcp.length}`]);
      body = <McpShop servers={data.mcp} />;
      break;
    case "skill":
      kind = "Skill";
      body = (
        <div className="wi-acts">
          <button className="danger" onClick={twice("del", () => act("del", () => RemoveSkill(picked.id).then(must), "Removed").then((done) => { if (done) onClose(); }))}><Trash2Icon size={14} />{t(confirm === "del" ? "Remove it?" : "Remove")}</button>
        </div>
      );
      break;
    case "desktop": {
      const dk = data.desktop;
      kind = "Desktop apps";
      if (dk) rows.push(["State", dk.ok ? "Ready" : dk.note], ["Tools", String(dk.tools)]);
      break;
    }
    case "mcp": {
      const m = data.mcp.find((x) => x.name === picked.id);
      kind = "MCP server";
      if (m) rows.push(["State", m.ok ? "Connected" : "Not connected"], ["Tools", String(m.tools)]);
      body = (
        <div className="wi-acts">
          <button className="danger" onClick={twice("del", () => act("del", () => RemoveMCPServer(picked.id).then(must), "Removed").then((done) => { if (done) onClose(); }))}><Trash2Icon size={14} />{t(confirm === "del" ? "Remove it?" : "Remove")}</button>
        </div>
      );
      break;
    }
    case "event": case "reminder": {
      const it = data.attention.find((a) => a.kind === picked.kind && (a.ref ?? a.title) === picked.id);
      kind = picked.kind === "event" ? "Meeting" : "Reminder";
      if (it?.at) rows.push(["When", when(it.at)]);
      if (it?.place) rows.push(["Where", it.place]);
      if (it?.detail) rows.push(["Detail", it.detail]);
      body = (
        <div className="wi-acts">
          <button onClick={() => open(PATHS.records, true)}>{t("Open Records")}</button>
          {picked.kind === "event" && (
            <button className="danger" onClick={twice("del", () => act("del", () => ForgetLife("schedule", picked.id), "Deleted").then(onClose))}><Trash2Icon size={14} />{t(confirm === "del" ? "Delete it?" : "Delete")}</button>
          )}
        </div>
      );
      break;
    }
    case "figure": {
      kind = "Live figure";
      const [who, fig] = picked.id.includes(":") ? picked.id.split(":") : ["", picked.id];
      const m = who ? data.meter.members.find((x) => x.name === who) : data.meter.total;
      const f = (n: number) => (n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(Math.round(n)));
      if (m) rows.push(["Tokens", f(m.tokens)], ["Tool calls", String(m.calls)], ["Rounds", String(m.rounds)], ["tok/s", String(Math.round(m.tokPerSec))], ["Failures", String(m.fails)]);
      void fig;
      body = <button className="wl-go ghost" onClick={() => open(PATHS.stats, true)}>{t("Open Stats")}<ArrowRightIcon size={14} /></button>;
      break;
    }
    case "settings": {
      // The queen's dais: what the hive's core is and where it runs, to read.
      kind = "Hive core";
      const h = data.hosts?.find((x) => x.agents.some(isCore));
      const core = h?.agents.find(isCore);
      if (h) rows.push(["Runs on", h.parent ? `${h.name} · ${data.hosts?.find((x) => x.id === h.parent)?.name ?? h.parent}` : h.name]);
      if (core?.version) rows.push(["Version", core.version]);
      const machines = data.hosts?.filter((x) => x.state === "live").length ?? 0;
      if (machines) rows.push(["Machines", String(machines)]);
      rows.push(["Workers", String(data.hive.members.length)]);
      if (core?.mcp?.length) rows.push(["MCP", core.mcp.join(", ")]);
      break;
    }
    case "host": {
      const h = data.hosts?.find((x) => x.id === picked.id);
      const roles = h ? haRoles(h, data.services ?? []) : { runs: [], standby: [] };
      const haNode = roles.runs.length + roles.standby.length > 0;
      kind = h ? kindLabel(h.kind, haNode) : "Host";
      if (!h) { body = <p className="wi-note">{t("This host is no longer listed.")}</p>; break; }
      if (h.hostname && h.hostname !== h.name) rows.push(["Hostname", h.hostname]);
      if (h.os || h.arch) rows.push(["System", [h.os, h.arch].filter(Boolean).join(" ")]);
      if (h.addresses?.length) rows.push([h.addresses.length > 1 ? "Addresses" : "Address", h.addresses.join(", ")]);
      if (h.parent) rows.push(["Inside", data.hosts?.find((x) => x.id === h.parent)?.name ?? h.parent]);
      // A host nothing has linked from, known as an HA service's node, is
      // where haify says; that is not "only in the settings".
      rows.push(["State", h.state === "configured" && haNode ? unlinkedStatus(h, data.services ?? [], t) : HOST_STATE[h.state] ?? h.state]);
      if (h.state !== "live" && h.lastSeen && !h.lastSeen.startsWith("0001")) rows.push(["Last seen", when(h.lastSeen)]);
      const svc = (data.services ?? []).filter((s) => isHost(h, s.active) || s.standbys.some((b) => isHost(h, b.host)));
      if (svc.length) rows.push(["HA services", svc.map((s) => `${s.name} (${isHost(h, s.active) ? t("active") : t("standby")})`).join(", ")]);
      body = <HostCard host={h} data={data} withControls onRun={(agent, prompt) => act("run", () => StartCLIRun(agent, prompt, "", "", true), "Run started")} busy={busy === "run"} />;
      break;
    }
    case "service": {
      const s = data.services?.find((x) => x.name === picked.id);
      kind = "HA service";
      if (!s) { body = <p className="wi-note">{t("This service is no longer listed.")}</p>; break; }
      if (s.vip) rows.push(["Service IP", s.vip]);
      rows.push(["Runs on", s.active || "—"], ["State", SVC_STATE[s.state] ?? s.state], ["Kept up by", s.provider || "—"]);
      if (s.units.length) rows.push(["Units", s.units.join(", ")]);
      body = <ServiceCard svc={s} hosts={data.hosts ?? []} />;
      break;
    }
    case "node": {
      kind = "Node";
      const on = data.hive.members.filter((m) => (m as any).node === picked.id);
      rows.push(["Workers", on.map((m) => m.name.replace(/^superleo-/, "")).join(", ") || "—"]);
      body = (
        <ul className="wi-list">
          {on.map((m) => <li key={m.name} onClick={() => focusInWorld("worker", m.name)}><i className={`wi-dot ${m.state === "live" ? "done" : "failed"}`} />{m.name}<em>{m.state}</em></li>)}
        </ul>
      );
      break;
    }
    case "records": {
      kind = "Today";
      const left = data.agenda.filter((e) => e.kind !== "timer").length;
      if (left) rows.push(["Still to come", String(left)]);
      body = (
        <>
          <ul className="wi-list wi-shop">
            {data.agenda.map((e) => (
              <li key={e.id}>
                <span className="wi-shop-n"><b>{e.title}</b><small>{[agendaWhen(e), t(KIND[e.kind])].filter(Boolean).join(" · ")}</small></span>
              </li>
            ))}
            {data.agenda.length === 0 && <li className="wi-none">{t("Nothing else planned today.")}</li>}
          </ul>
          <div className="wi-acts">
            <button onClick={() => open(PATHS.records, true)}>{t("Open Records")}</button>
          </div>
          <Talk who={{ kind: "queen" }} placeholder="Tell noted something…" />
        </>
      );
      break;
    }
    default: {
      const it = data.attention.find((a) => (a.ref ?? "") === picked.id && a.kind === picked.kind);
      const label: Record<string, string> = { approval: "Approval required", bee: "Waiting for your reply", report: "From a bee", failed: "Task failed", lost: "Worker offline", run: "Coding run failed" };
      kind = label[picked.kind] ?? "Needs you";
      if (it?.kind === "report") rows.push(["Reports", String(it.updates ?? 1)]);
      if (it?.detail) rows.push([it.kind === "approval" ? "Command" : "Latest", it.detail]);
      if (it?.kind === "approval") {
        body = (
          <div className="wl-two">
            <button className="wl-go" disabled={!!busy} onClick={() => act("y", () => ResolveToolApproval(it.ref ?? "", true), "Allowed").then(onClose)}>{t("Allow once")}</button>
            <button className="wl-go ghost" disabled={!!busy} onClick={() => act("n", () => ResolveToolApproval(it.ref ?? "", false), "Denied").then(onClose)}>{t("Deny")}</button>
          </div>
        );
      } else if (it?.kind === "report" || it?.kind === "bee") {
        body = (
          <>
            {it.kind === "bee" && say("Answer it…", (s) => WakeStandingAgent(it.ref ?? "", s), "Sent")}
            <button className="wl-go" onClick={() => focusInWorld("bee", it.ref ?? "")}>{t("Go to the bee")}<ArrowRightIcon size={14} /></button>
          </>
        );
      } else if (it?.kind === "lost") {
        body = <button className="wl-go" onClick={() => focusInWorld("worker", it.ref ?? "")}>{t("Go to the worker")}<ArrowRightIcon size={14} /></button>;
      } else if (it?.kind === "failed") {
        body = <FailedOrder id={it.ref ?? ""} open={() => open(taskPath(it.ref ?? ""))} />;
      } else if (it) {
        body = <button className="wl-go" onClick={() => open(attentionPath(it))}>{t("Open details")}<ArrowRightIcon size={14} /></button>;
      }
    }
  }

  // Every thing has a page with all it can do; it opens full size, as its
  // screen, over the world.
  const fullPaths: Record<string, string> = {
    hall: PATHS.home, worker: PATHS.hive, node: PATHS.hive, order: taskPath(picked.id),
    bee: PATHS.agents + "?agent=" + encodeURIComponent(picked.id), machine: PATHS.coding, cli: PATHS.coding, agent: PATHS.coding,
    brain: PATHS.knowledge, skills: PATHS.skills, skill: PATHS.skills, mcphub: PATHS.mcp, mcp: PATHS.mcp,
    dashboard: PATHS.dashboards, records: PATHS.records, event: PATHS.records, reminder: PATHS.records,
    figure: PATHS.stats, settings: PATHS.settings, host: PATHS.hive, service: PATHS.hive,
  };
  const screen = fullPaths[picked.kind] ?? PATHS.tasks;
  return (
    <aside className="wl-detail">
      <header>
        <div><em>{t(kind)}</em><b>{picked.title}</b></div>
        {picked.kind !== "settings" && !full && <button title={t("Open full screen")} onClick={() => ["worker", "agent", "cli"].includes(picked.kind) ? setFull(true) : open(screen, true)}><Maximize2Icon size={14} /></button>}
        <button title={t("Close")} onClick={() => full ? setFull(false) : onClose()}><XIcon size={15} /></button>
      </header>
      {rows.length > 0 && (
        <div className="wl-facts">
          <button type="button" className="wl-facts-toggle" aria-expanded={facts} onClick={() => setFacts(!facts)} data-testid="facts-toggle">
            {facts ? <ChevronDownIcon size={13} /> : <ChevronRightIcon size={13} />}
            <span>{facts ? t("Details") : rows.slice(0, 3).map(([k, v]) => `${t(k)} ${t(v)}`).join(" · ")}</span>
          </button>
          {(facts || picked.kind === "settings") && <dl>{rows.map(([k, v]) => <div key={k}><dt>{t(k)}</dt><dd>{t(v)}</dd></div>)}</dl>}
        </div>
      )}
      <div className="wi-body">{body}</div>
    </aside>
  );
}

/** A bee's newest report, read when the bee is selected. */
function Latest({ id }: { id: string }) {
  const { t } = useI18n();
  const [r, setR] = useState<{ at: string; message: string } | null>(null);
  useEffect(() => {
    let alive = true;
    StandingReports(id).then((l: any[]) => { if (alive) setR(l?.[0] ? { at: String(l[0].at ?? ""), message: String(l[0].message ?? "") } : null); }).catch(() => {});
    return () => { alive = false; };
  }, [id]);
  if (!r) return null;
  return <div className="wi-latest"><em>{t("Last said")} · {when(r.at)}</em><p>{r.message}</p></div>;
}

/** What a failed order said, and a way to the whole log. */
function FailedOrder({ id, open }: { id: string; open: () => void }) {
  const { t } = useI18n();
  const [why, setWhy] = useState("");
  useEffect(() => {
    HiveTaskDetail(id).then((d: any) => setWhy(String(d?.error || d?.result || ""))).catch(() => {});
  }, [id]);
  return (
    <>
      {why && <pre className="wi-text">{why}</pre>}
      <button className="wl-go" onClick={open}>{t("Open full log")}<ArrowRightIcon size={14} /></button>
    </>
  );
}

/** Start a coding run on this machine: which CLI, and what to do. */
function NewRun({ clis, host, onStart, busy }: { clis: string[]; host: string; onStart: (agent: string, prompt: string) => Promise<unknown>; busy: boolean }) {
  const { t } = useI18n();
  const [cli, setCli] = useState(clis[0]);
  const [prompt, setPrompt] = useState("");
  const agent = host === "core" ? cli : `${cli}.${host}`;
  return (
    <form className="wi-run" onSubmit={(e) => { e.preventDefault(); if (prompt.trim()) void onStart(agent, prompt.trim()).then(() => setPrompt("")); }}>
      <div className="wi-clis">{clis.slice(0, 6).map((c) => <button type="button" key={c} className={c === cli ? "on" : ""} onClick={() => setCli(c)}>{c}</button>)}</div>
      <textarea value={prompt} onChange={(e) => setPrompt(e.target.value)} rows={2} placeholder={t("What should {cli} do here?", { cli })} />
      <button className="wl-go" type="submit" disabled={busy || !prompt.trim()}>{t("Start run")}<PlayIcon size={13} /></button>
    </form>
  );
}

/** Skills on this machine that can be installed, found by what they do; an
 *  install is one click and lands on the rack (and, through the hive, on
 *  every worker). */
function SkillShop({ installed }: { installed: string[] }) {
  const { t } = useI18n();
  const [q, setQ] = useState("");
  const [found, setFound] = useState<{ name: string; description: string; path: string; installed: boolean }[] | null>(null);
  const [busy, setBusy] = useState("");
  const search = async (query: string) => {
    setBusy("search");
    try { setFound(((await SearchSkills(query)) ?? []) as any); } catch (e) { toast.error(String(e)); } finally { setBusy(""); }
  };
  useEffect(() => { void search(""); }, []);
  return (
    <>
      <form className="wi-say" onSubmit={(e) => { e.preventDefault(); void search(q.trim()); }}>
        <input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t("Find a skill by what it does…")} />
        <button type="submit" disabled={busy === "search"}><SearchIcon size={14} /></button>
      </form>
      <ul className="wi-list wi-shop">
        {(found ?? []).slice(0, 8).map((c) => {
          const have = installed.includes(c.name) || c.installed;
          return (
            <li key={c.path || c.name} title={c.description}>
              <span className="wi-shop-n"><b>{c.name}</b><small>{c.description}</small></span>
              <button className="wi-get" disabled={have || busy === c.name} onClick={async () => {
                setBusy(c.name);
                try {
                  const r = await InstallSkill(c.name, c.path);
                  if (r && r !== "ok") throw new Error(r);
                  toast.success(t("Installed {name}", { name: c.name }));
                  setFound((cur) => cur?.map((x) => (x.name === c.name ? { ...x, installed: true } : x)) ?? null);
                } catch (e) { toast.error(String(e instanceof Error ? e.message : e)); } finally { setBusy(""); }
              }}>{have ? t("Installed") : busy === c.name ? "…" : t("Install")}</button>
            </li>
          );
        })}
        {found && found.length === 0 && <li className="wi-none">{t("Nothing found on this machine.")}</li>}
      </ul>
    </>
  );
}

/** MCP servers from the official registry: search by what they should do,
 *  install with one click; each one becomes a box cabled to the hub. */
function McpShop({ servers }: { servers: WorldData["mcp"] }) {
  const { t } = useI18n();
  const [q, setQ] = useState("");
  const [found, setFound] = useState<{ name: string; title?: string; description: string; command?: string; args?: string[] }[] | null>(null);
  const [busy, setBusy] = useState("");
  const short = (full: string) => (full.split("/").pop() || full).replace(/[^A-Za-z0-9._-]/g, "-");
  const search = async () => {
    if (!q.trim()) return;
    setBusy("search");
    try {
      const r: any = await SearchMCPServers(q.trim());
      if (r?.error) toast.error(r.error);
      setFound(Array.isArray(r?.servers) ? r.servers : []);
    } catch (e) { toast.error(String(e)); } finally { setBusy(""); }
  };
  return (
    <>
      {servers.length > 0 && (
        <ul className="wi-list">
          {servers.map((m) => (
            <li key={m.name} onClick={() => focusInWorld("mcp", m.name)}><i className={`wi-dot ${m.ok ? "done" : "failed"}`} />{m.name}<em>{m.ok ? `${m.tools} tools` : t("not connected")}</em></li>
          ))}
        </ul>
      )}
      <form className="wi-say" onSubmit={(e) => { e.preventDefault(); void search(); }}>
        <input value={q} onChange={(e) => setQ(e.target.value)} placeholder={t("What should it be able to do? e.g. postgres, slack")} />
        <button type="submit" disabled={!q.trim() || busy === "search"}><SearchIcon size={14} /></button>
      </form>
      {busy === "search" && <p className="wi-note">{t("Searching…")}</p>}
      {found && (
        <ul className="wi-list wi-shop">
          {found.slice(0, 8).map((c) => {
            const name = short(c.name);
            const have = servers.some((x) => x.name === name);
            return (
              <li key={c.name} title={c.description}>
                <span className="wi-shop-n"><b>{c.title || name}</b><small>{c.description}</small></span>
                <button className="wi-get" disabled={have || !c.command || busy === c.name} title={c.command ? "" : t("Hosted remotely; add it by URL instead.")} onClick={async () => {
                  setBusy(c.name);
                  try {
                    const r = await InstallMCPServer(name, c.command || "", c.args || [], {});
                    if (r !== "ok") throw new Error(r);
                    toast.success(t("Installed {name}", { name }));
                  } catch (e) { toast.error(String(e instanceof Error ? e.message : e)); } finally { setBusy(""); }
                }}>{have ? t("Installed") : busy === c.name ? "…" : t("Install")}</button>
              </li>
            );
          })}
          {found.length === 0 && <li className="wi-none">{t("No servers matched.")}</li>}
        </ul>
      )}
    </>
  );
}

/** The sessions people run in a machine's terminals, by coding CLI: each by
 *  its project, what it is on, and since when. Only what the machine sent —
 *  never a prompt. */
function TermSessions({ clis }: { clis: Record<string, CCActivity> }) {
  const { t } = useI18n();
  const order = Object.keys(clis).filter((c) => clis[c]?.sessions?.length)
    .sort((x, y) => (clis[y].working + clis[y].waiting) - (clis[x].working + clis[x].waiting) || x.localeCompare(y));
  if (!order.length) return null;
  const since = (s: string) => {
    const d = new Date(s);
    return isNaN(+d) ? "" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  };
  return (
    <div className="wi-cc">
      <p className="wi-cap">{t("Terminal sessions")}</p>
      {order.map((c) => {
        const act = clis[c] as CLIActivity;
        return (
          <div key={c} className="wi-cc-cli">
            <p className="wi-cc-name">{c}{act.source === "process" && <em>{t("by its processes")}</em>}</p>
            <ul className="wi-list">
              {act.sessions.slice(0, 8).map((s) => (
                <li key={s.id} title={s.tools ? t("{count} tool calls", { count: s.tools }) : undefined}>
                  <i className={`wi-dot ${s.state === "working" ? "running" : s.state === "waiting" ? "waiting" : ""}`} />
                  {sessionLine(s, c)}
                  <em>{t("since {time}", { time: since(s.since) })}</em>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </div>
  );
}

/** One coding run in a machine's card: its prompt, and what it came to when
 *  opened. */
function RunLine({ run, onStop }: { run: { id: string; prompt: string; state: string }; onStop: () => void }) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const [out, setOut] = useState("");
  useEffect(() => {
    if (!open || out) return;
    CLIRunDetail(run.id).then((r: any) => setOut(String(r?.summary || r?.error || (run.state === "running" ? t("Still running…") : "")))).catch((e) => setOut(String(e)));
  }, [open]);
  return (
    <>
      <li onClick={() => setOpen((v) => !v)}><i className={`wi-dot ${run.state}`} />{run.prompt}
        {run.state === "running" && <button className="wi-x" title={t("Stop")} onClick={(e) => { e.stopPropagation(); onStop(); }}><SquareIcon size={11} /></button>}
      </li>
      {open && out && <pre className="wi-text">{out}</pre>}
    </>
  );
}

const AGENT_STATE: Record<string, string> = {
  live: "done", working: "running", waiting: "waiting", idle: "", installed: "", lost: "failed", configured: "", standby: "", "standby-unready": "failed",
};
/** What a role or a state reads as, short. */
const roleLabel = (r?: string) => (r === "queen" ? "queen" : r === "worker" ? "worker" : r === "machine" ? "machine agent" : r === "relay" ? "relay" : r === "standalone" ? "on its own" : r || "");

/**
 * A host's agents in three groups, each a click away in the world: the
 * SuperLeos (with their part in the hive and the MCP servers they can call),
 * the coding CLIs, the named agents (with the SuperLeo that runs them and the
 * HA service that keeps them up); the hosts inside it; and, for a machine
 * agent's host or the core's, the machine itself to look after.
 */
function HostCard({ host, data, withControls, onRun, busy }: { host: Host; data: WorldData; withControls: boolean; onRun?: (agent: string, prompt: string) => Promise<unknown>; busy?: boolean }) {
  const { t } = useI18n();
  const sup = host.agents.filter((a) => a.kind === "superleo");
  const clis = host.agents.filter((a) => a.kind === "cli");
  const named = host.agents.filter((a) => a.kind === "agent");
  const inside = (data.hosts ?? []).filter((x) => x.parent === host.id);
  const machine = sup.find((a) => a.role === "machine");
  const core = sup.some(isCore);
  // What to look after: core's own machine, or the machine agent's.
  const care = core ? "core" : machine?.name;
  const runVia = core ? "core" : machine?.name ?? clis[0]?.via;
  const stateWord = (a: HostAgent) => t(a.state === "standby-unready" ? "standby · not ready" : a.state);
  const focusOf = (a: HostAgent) => {
    if (a.kind === "superleo") return isCore(a) ? focusInWorld("hall", "queen") : focusInWorld("worker", a.name);
    if (a.kind === "cli") return focusInWorld("cli", core ? cliName(a) : `${cliName(a)}.${a.via}`);
    if (isStandby(a)) return focusInWorld("service", a.service || a.name);
    return focusInWorld("agent", `${a.name}.${a.via || host.name}`);
  };
  const row = (a: HostAgent, name: string, tail: string) => (
    <li key={a.kind + a.name} onClick={() => focusOf(a)} title={a.about || undefined}>
      <i className={`wi-dot ${AGENT_STATE[a.state] ?? ""}`} />{name}<em>{tail}</em>
    </li>
  );
  return (
    <div className="wi-host">
      {sup.length > 0 && <>
        <p className="wi-cap">SuperLeo</p>
        <ul className="wi-list">
          {sup.flatMap((a) => [
            row(a, a.name, [t(roleLabel(a.role)), a.version?.split("-")[0]].filter(Boolean).join(" · ")),
            (a.mcp?.length ?? 0) > 0 ? <li key={a.name + ":mcp"} className="wi-host-mcp">MCP · {a.mcp!.join(", ")}</li> : null,
          ])}
        </ul>
      </>}
      {clis.length > 0 && <>
        <p className="wi-cap">{t("Coding CLIs")}</p>
        <ul className="wi-list">{clis.map((a) => row(a, cliName(a), stateWord(a)))}</ul>
      </>}
      {named.length > 0 && <>
        <p className="wi-cap">{t("Agents")}</p>
        <ul className="wi-list">{named.map((a) => row(a, a.name, [a.via ? t("via {name}", { name: a.via }) : "", a.activity ? t(a.activity) : stateWord(a)].filter(Boolean).join(" · ")))}</ul>
      </>}
      {inside.length > 0 && <>
        <p className="wi-cap">{t("Inside")}</p>
        <ul className="wi-list">
          {inside.map((x) => (
            <li key={x.id} onClick={() => focusInWorld("host", x.id)}>
              <i className={`wi-dot ${x.state === "live" ? "done" : x.state === "lost" ? "failed" : ""}`} />{x.name}
              <em>{[t(kindLabel(x.kind)), x.agents.filter((a) => a.kind === "superleo").map((a) => t(roleLabel(a.role))).join(", ")].filter(Boolean).join(" · ")}</em>
            </li>
          ))}
        </ul>
      </>}
      {host.agents.length === 0 && inside.length === 0 && <p className="wi-note">{t("Nothing runs here that this core knows of.")}</p>}
      {withControls && care && host.state === "live" && <MachineHost name={care} />}
      {withControls && onRun && runVia && clis.length > 0 && (
        <NewRun clis={clis.map(cliName)} host={runVia} onStart={onRun} busy={!!busy} />
      )}
    </div>
  );
}

/** An HA service's hosts: where it runs, where it can go and whether each is
 *  ready to take it, and the agents it keeps up. */
function ServiceCard({ svc, hosts }: { svc: HAService; hosts: Host[] }) {
  const { t } = useI18n();
  const find = (name: string) => hosts.find((h) => isHost(h, name));
  const agents = hosts.flatMap((h) => h.agents.filter((a) => a.service === svc.name && !isStandby(a)).map((a) => ({ a, h })));
  const go = (name: string) => { const h = find(name); if (h) focusInWorld("host", h.id); };
  return (
    <div className="wi-host">
      <p className="wi-cap">{t("Hosts")}</p>
      <ul className="wi-list">
        {svc.active && <li onClick={() => go(svc.active!)}><i className="wi-dot done" />{svc.active}<em>{t("active")}</em></li>}
        {svc.standbys.map((b) => (
          <li key={b.host} onClick={() => go(b.host)}><i className={`wi-dot ${b.ready ? "" : "failed"}`} />{b.host}<em>{t(b.ready ? "standby, ready" : "standby, not ready")}</em></li>
        ))}
        {!svc.active && svc.standbys.length === 0 && <li className="wi-none">{t("No hosts.")}</li>}
      </ul>
      {agents.length > 0 && <>
        <p className="wi-cap">{t("Agents")}</p>
        <ul className="wi-list">
          {agents.map(({ a, h }) => (
            <li key={a.name + h.id} onClick={() => focusInWorld("agent", `${a.name}.${a.via || h.name}`)}><i className="wi-dot done" />{a.name}<em>{t("on {name}", { name: h.name })}</em></li>
          ))}
        </ul>
      </>}
    </div>
  );
}
