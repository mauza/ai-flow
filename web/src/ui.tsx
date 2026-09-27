import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import {
  Bot,
  CheckCircle2,
  CircleAlert,
  CircleCheck,
  CircleDot,
  CircleX,
  GitBranch,
  GitPullRequest,
  Hand,
  ListChecks,
  Loader2,
  MessageSquareText,
  Pause,
  Split,
  X,
  Zap,
} from "lucide-react";

export const typeMeta: Record<string, { color: string; icon: typeof Bot; label: string; blurb: string }> = {
  llm: { color: "var(--t-llm)", icon: MessageSquareText, label: "LLM", blurb: "One structured model call, no tools" },
  agent: { color: "var(--t-agent)", icon: Bot, label: "Agent", blurb: "pi coding agent with granted tools" },
  check: { color: "var(--t-check)", icon: ListChecks, label: "Check", blurb: "Shell command; exit code picks the outcome" },
  gate: { color: "var(--t-gate)", icon: Hand, label: "Gate", blurb: "Waits for a human decision" },
  switch: { color: "var(--t-switch)", icon: Split, label: "Switch", blurb: "Routes on a CEL expression" },
  action: { color: "var(--t-action)", icon: Zap, label: "Action", blurb: "Built-in action (open a PR, comment)" },
};

export function TypeIcon({ type, size }: { type: string; size?: number }) {
  const m = typeMeta[type] ?? { color: "var(--muted)", icon: CircleDot };
  const Icon = m.icon;
  return (
    <span className="fnode-icon" style={{ ["--type-color" as string]: m.color, width: size, height: size }}>
      <Icon />
    </span>
  );
}

const statusLabels: Record<string, string> = {
  plan_failed: "needs attention",
  flow_ready: "ready",
  succeeded: "succeeded",
};

export function Pill({ status, label }: { status: string; label?: string }) {
  return (
    <span className={`pill ${status}`}>
      <span className="dot" />
      {label ?? statusLabels[status] ?? status.replace(/_/g, " ")}
    </span>
  );
}

export function StatusIcon({ status }: { status: string }) {
  switch (status) {
    case "succeeded":
      return <CheckCircle2 />;
    case "error":
    case "failed":
      return <CircleX />;
    case "running":
    case "pending":
      return <Loader2 className="spin" style={{ animation: "spin 1.2s linear infinite" }} />;
    case "waiting":
      return <Pause />;
    case "canceled":
      return <CircleX />;
  }
  return <CircleDot />;
}

export function Spinner({ lg }: { lg?: boolean }) {
  return <span className={`spinner${lg ? " lg" : ""}`} />;
}

export function Empty({ icon: Icon, title, children }: { icon: typeof Bot; title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <Icon />
      <h3>{title}</h3>
      {children}
    </div>
  );
}

export function Modal({ title, onClose, children, footer }: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal" role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head">
          <h2>{title}</h2>
          <span className="spacer" />
          <button className="btn ghost icon sm" onClick={onClose} aria-label="Close">
            <X />
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>
  );
}

// ---- toasts ----

interface Toast {
  id: number;
  kind: "ok" | "error";
  text: string;
}
const ToastCtx = createContext<(kind: Toast["kind"], text: string) => void>(() => {});

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const push = useCallback((kind: Toast["kind"], text: string) => {
    const id = Date.now() + Math.random();
    setToasts((t) => [...t, { id, kind, text }]);
    window.setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), kind === "error" ? 7000 : 3500);
  }, []);
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toast-wrap" aria-live="polite">
        {toasts.map((t) => (
          <div key={t.id} className={`toast ${t.kind}`}>
            {t.kind === "ok" ? <CircleCheck /> : <CircleAlert />}
            <span>{t.text}</span>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  );
}

export const useToast = () => useContext(ToastCtx);

export function SourceChip({ source, identifier, url }: { source: string; identifier?: string; url?: string }) {
  if (source === "linear") {
    const inner = (
      <>
        <LinearMark />
        {identifier || "Linear"}
      </>
    );
    return url ? (
      <a className="chip" href={url} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()}>
        {inner}
      </a>
    ) : (
      <span className="chip">{inner}</span>
    );
  }
  return <span className="chip">manual</span>;
}

export function LinearMark() {
  return (
    <svg viewBox="0 0 100 100" width="12" height="12" aria-hidden="true">
      <path
        fill="currentColor"
        d="M1.2 61.5c-.2-.9.9-1.5 1.6-.8l36.5 36.5c.7.7.1 1.8-.8 1.6C20 94.4 5.6 80 1.2 61.5ZM0 46.9c0 .4.1.7.4 1l51.7 51.7c.3.3.6.4 1 .4 2.3-.1 4.6-.4 6.8-.9.8-.2 1-1.1.5-1.6L2.4 39.6c-.5-.5-1.5-.3-1.6.5-.4 2.2-.7 4.5-.8 6.8Zm4.1-16.9c-.2.4-.1.9.2 1.2L68.8 95.7c.3.3.8.4 1.2.2 1.7-.8 3.4-1.6 5-2.6.6-.3.6-1.1.2-1.5L8.2 25c-.4-.4-1.2-.4-1.5.2-1 1.6-1.8 3.3-2.6 5ZM12.7 18c-.3-.3-.3-.8 0-1.1C21.9 6.5 35.3 0 50.2 0 77.7 0 100 22.3 100 49.8c0 14.9-6.5 28.3-16.9 37.5-.3.3-.8.3-1.1 0L12.7 18Z"
      />
    </svg>
  );
}

export function PRLink({ url }: { url?: string }) {
  if (!url) return null;
  const n = url.split("/").pop();
  return (
    <a className="chip accent" href={url} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()}>
      <GitPullRequest />
      PR #{n}
    </a>
  );
}

export function BranchChip({ branch }: { branch: string }) {
  return (
    <span className="chip mono" title={branch}>
      <GitBranch />
      {branch}
    </span>
  );
}
