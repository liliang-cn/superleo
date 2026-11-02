import { useState } from "react";
import { translate } from "../lib/i18n";
import { setBrowserMode, useBrowserStatus, useBrowserSwitching } from "../lib/browser";
import { Row, Seg } from "./settings/kit";

/** Copies a value and says so for a moment. */
function CopyButton({ value, label }: { value: string; label: string }) {
  const [done, setDone] = useState(false);
  return (
    <button type="button" className="st-btn" onClick={() => {
      void navigator.clipboard?.writeText(value).then(() => {
        setDone(true);
        window.setTimeout(() => setDone(false), 1500);
      }).catch(() => {});
    }}>
      {translate(done ? "Copied" : label)}
    </button>
  );
}

const MODES = [
  { value: "off", label: "Off" },
  { value: "chrome", label: "My Chrome" },
  { value: "own", label: "Separate browser" },
] as const;

type Mode = (typeof MODES)[number]["value"];

/**
 * Which browser the agent drives: none, the person's own Chrome (with their
 * sign-ins), or a separate one agent-browser launches. Applied at once, not
 * with the page's Save: the backend rebuilds around it, and the line under
 * the name follows it until the browser is up.
 */
export default function BrowserModeCard({ onChanged }: { onChanged?: (mode: string) => void }) {
  const st = useBrowserStatus();
  const switching = useBrowserSwitching();
  const [err, setErr] = useState("");
  const current = (switching || (st?.enabled ? st.mode : "off")) as Mode;
  const pick = async (mode: Mode) => {
    if (mode === current) return;
    setErr("");
    try {
      await setBrowserMode(mode);
      onChanged?.(mode);
    } catch (e) {
      setErr(String(e instanceof Error ? e.message : e));
    }
  };
  const state = err || (!st ? "" : switching || (st.enabled && !st.ready && !st.error) ? translate("Installing browser…")
    : st.error ? st.error : st.enabled && st.ready && st.version ? `agent-browser ${st.version}` : "");
  const bad = !!err || !!st?.error;
  const bridge = current === "chrome" ? st?.bridge : undefined;
  return (
    <>
      <Row label="Browser" testId="browser-mode-card"
        sub={state ? <span className={bad ? "st-note bad" : undefined}>{state}</span> : undefined}>
        <Seg label="Browser" value={current} options={[...MODES]} disabled={!!switching} onChange={(m) => void pick(m)} />
      </Row>
      {bridge && (
        <>
          <Row label="Chrome extension" testId="browser-bridge-status"
            sub={<span className={bridge.connected ? "st-note" : "st-note bad"}>{translate(bridge.connected ? "Connected" : "Not connected")}{bridge.connected && bridge.browser ? ` · ${bridge.browser}` : ""}</span>} />
          {!bridge.connected && (
            <>
              <Row label="1. Load unpacked" mono sub={bridge.dir} testId="browser-bridge-dir">
                <CopyButton value={bridge.dir} label="Copy path" />
              </Row>
              <Row label="2. Paste code" testId="browser-bridge-code">
                <CopyButton value={bridge.code} label="Copy code" />
              </Row>
            </>
          )}
        </>
      )}
    </>
  );
}
