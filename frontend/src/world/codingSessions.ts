/** The conversations of one coding agent, for its card. */
export type CodingConv = {
  id: string; kind: string; name?: string; agent?: string; machine?: string; title: string;
  at: string; state?: string; source?: string; cwd?: string;
};

/** "claude.mac" is the claude CLI on the machine mac; "claude" is the core's. */
export function splitAgent(id: string): { cli: string; machine: string } {
  const i = id.indexOf(".");
  return i < 0 ? { cli: id, machine: "" } : { cli: id.slice(0, i), machine: id.slice(i + 1) };
}

const stamp = (c: CodingConv) => { const n = Date.parse(c.at); return isNaN(n) ? 0 : n; };

/** One coding agent's conversations on its own machine, runs and terminal
 *  sessions alike, newest first. */
export function sessionsOf(items: CodingConv[], agent: string): CodingConv[] {
  const { cli, machine } = splitAgent(agent);
  return items
    .filter((c) => c.kind === "coding" && (c.agent || c.name || "") === cli && (c.machine ?? "") === machine)
    .sort((a, b) => stamp(b) - stamp(a));
}

/** The last folder of where a conversation works. */
export function folderOf(c: { cwd?: string }): string {
  const parts = (c.cwd ?? "").split(/[\\/]/).filter(Boolean);
  return parts.length ? parts[parts.length - 1] : "";
}
