import type React from "react";
import { BoxIcon, ListIcon, MessageSquareIcon, MessagesSquareIcon, SlidersHorizontalIcon } from "lucide-react";
import { useI18n } from "../lib/i18n";
import { queenRef, useQueenName } from "../lib/queenName";
import "./fabs.css";

/**
 * The round buttons down the bottom right of every screen: the way between
 * 3D and Menu above, the conversation with the queen below it, where the
 * thumb and the pointer reach first.
 */
export function FabStack({ children, style, lifted }: { children: React.ReactNode; style?: React.CSSProperties; lifted?: boolean }) {
  return <div className={lifted ? "fab-stack lifted" : "fab-stack"} style={style}>{children}</div>;
}

export function ChatFab({ onClick }: { onClick: () => void }) {
  const { t } = useI18n();
  const queen = useQueenName();
  const label = t("Talk to {name}", { name: queenRef(queen) });
  return (
    <button type="button" className="fab fab-chat" onClick={onClick} aria-label={label} title={label}>
      <MessageSquareIcon size={18} />
    </button>
  );
}

/** Says where it goes, not where you are. */
export function ModeFab({ world, onClick }: { world: boolean; onClick: () => void }) {
  const { t } = useI18n();
  const label = world ? t("Menus and lists, the way it was") : t("The hive in 3D");
  return (
    <button type="button" className="fab fab-mode" onClick={onClick} aria-label={label} title={label}>
      {world ? <ListIcon size={17} /> : <BoxIcon size={17} />}
    </button>
  );
}

/** Settings, in 3D where the rail that holds it is not drawn. */
export function SettingsFab({ onClick }: { onClick: () => void }) {
  const { t } = useI18n();
  return (
    <button type="button" className="fab fab-mode" onClick={onClick} aria-label={t("Settings")} title={t("Settings")}>
      <SlidersHorizontalIcon size={17} />
    </button>
  );
}

/** Every conversation, in 3D where the rail that holds the page is not drawn. */
export function ConversationsFab({ onClick }: { onClick: () => void }) {
  const { t } = useI18n();
  return (
    <button type="button" className="fab fab-mode" onClick={onClick} aria-label={t("Conversations")} title={t("Conversations")}>
      <MessagesSquareIcon size={17} />
    </button>
  );
}
