import { useEffect, useRef, useState } from "react";
import { Check, Eraser, Send, Sparkles, X } from "lucide-react";
import { api, type FileChange, type MapMessage } from "../api";
import { useResource } from "../hooks";
import { Spinner, useToast } from "../ui";
import Markdown from "../product/Markdown";
import DiffView from "../product/DiffView";

const SUGGESTIONS = [
  "What is missing from the MVP release?",
  "Write acceptance criteria for tasks that have none",
  "Split the biggest user task into smaller ones",
  "Which tasks should move a metric but don't say so?",
];

/** Ask an LLM about the map; it can propose file changes you apply locally. */
export default function Assistant({ project, mapID, onApplied, onClose }: { project: string; mapID: string; onApplied: () => void; onClose: () => void }) {
  const history = useResource(() => api.mapChat(project, mapID), [project, mapID]);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [applied, setApplied] = useState<Set<number>>(new Set());
  const log = useRef<HTMLDivElement>(null);
  const toast = useToast();
  const msgs = history.data ?? [];

  useEffect(() => {
    log.current?.scrollTo({ top: log.current.scrollHeight });
  }, [msgs.length, busy]);

  const ask = async (message: string) => {
    if (!message.trim()) return;
    setBusy(true);
    setText("");
    try {
      await api.askMap(project, mapID, message);
      history.reload();
    } catch (e) {
      toast("error", (e as Error).message);
      setText(message);
    } finally {
      setBusy(false);
    }
  };
  const apply = async (i: number, changes: FileChange[]) => {
    try {
      const r = await api.applyChanges(project, mapID, changes);
      setApplied((s) => new Set(s).add(i));
      toast("ok", r.problems.length ? `Applied with ${r.problems.length} problem(s) to fix` : "Applied (not committed)");
      onApplied();
    } catch (e) {
      toast("error", (e as Error).message);
    }
  };
  const clear = async () => {
    if (!window.confirm("Clear this conversation?")) return;
    await api.clearMapChat(project, mapID);
    history.reload();
  };

  return (
    <aside className="map-panel assistant" aria-label="Map assistant">
      <div className="map-panel-head">
        <Sparkles size={16} />
        <h2 className="grow">Assistant</h2>
        <button className="btn ghost icon sm" onClick={clear} aria-label="Clear conversation" title="Clear">
          <Eraser />
        </button>
        <button className="btn ghost icon sm" onClick={onClose} aria-label="Close">
          <X />
        </button>
      </div>
      <div className="map-panel-body chat-log" ref={log}>
        {msgs.length === 0 && !busy && (
          <div className="stack">
            <p className="small muted">Ask about this map, or ask for changes. Proposed changes show as diffs; nothing is written until you apply, and nothing is committed until you commit.</p>
            {SUGGESTIONS.map((s) => (
              <button key={s} className="btn sm suggestion" onClick={() => ask(s)}>
                {s}
              </button>
            ))}
          </div>
        )}
        {msgs.map((m: MapMessage, i) => (
          <div key={i} className={`msg ${m.role}`}>
            {m.role === "assistant" ? <Markdown source={m.content} /> : <p>{m.content}</p>}
            {m.changes && m.changes.length > 0 && <Changes project={project} changes={m.changes} issues={m.issues ?? []} applied={applied.has(i)} onApply={() => apply(i, m.changes!)} />}
          </div>
        ))}
        {busy && (
          <div className="msg assistant row small muted">
            <Spinner /> Thinking…
          </div>
        )}
      </div>
      <form className="map-panel-foot" onSubmit={(e) => (e.preventDefault(), ask(text))}>
        <textarea
          className="input"
          rows={2}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && !e.shiftKey && (e.preventDefault(), ask(text))}
          placeholder="Ask about the map…"
          aria-label="Message"
          disabled={busy}
        />
        <button className="btn primary icon" disabled={busy || !text.trim()} aria-label="Send">
          <Send />
        </button>
      </form>
    </aside>
  );
}

function Changes({ project, changes, issues, applied, onApply }: { project: string; changes: FileChange[]; issues: string[]; applied: boolean; onApply: () => void }) {
  const [before, setBefore] = useState<Record<string, string>>({});
  useEffect(() => {
    for (const c of changes)
      api.readFile(project, c.path).then(
        (f) => setBefore((b) => ({ ...b, [c.path]: f.content })),
        () => setBefore((b) => ({ ...b, [c.path]: "" })),
      );
  }, [project, changes]);
  return (
    <div className="proposal">
      <div className="label">Proposed changes</div>
      {changes.map((c) => (
        <details key={c.path} open={changes.length === 1}>
          <summary className="mono small">
            {c.content === null ? "delete " : ""}
            {c.path.replace(/^product\/user-story-maps\/[^/]+\//, "")}
          </summary>
          <DiffView before={before[c.path] ?? ""} after={c.content ?? ""} />
        </details>
      ))}
      {issues.length > 0 && (
        <div className="error-box small">
          Still has problems:
          <ul>
            {issues.map((i) => (
              <li key={i}>{i}</li>
            ))}
          </ul>
        </div>
      )}
      <button className="btn sm" disabled={applied} onClick={onApply}>
        <Check />
        {applied ? "Applied" : "Apply"}
      </button>
    </div>
  );
}
