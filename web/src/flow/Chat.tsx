import { useEffect, useMemo, useRef, useState } from "react";
import { diffLines } from "diff";
import { Check, Send, Sparkles, X } from "lucide-react";
import { api, type ChatMessage, type ChatResult } from "../api";
import { useResource } from "../hooks";
import { timeAgo } from "../format";
import { Spinner, useToast } from "../ui";

const SUGGESTIONS = [
  "Add a code review step before opening the PR",
  "Use a smaller model for the review",
  "Add a human approval before the PR",
  "Run the whole test suite, not just one file",
  "Let the fix step retry up to 4 times",
];

export default function Chat({ name, yaml, onApply }: { name: string; yaml: string; onApply: (yaml: string) => void }) {
  const history = useResource(() => api.chat(name), [name], (e) => e.type === "flow" && e.id === name);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [proposal, setProposal] = useState<ChatResult | null>(null);
  const [base, setBase] = useState("");
  const log = useRef<HTMLDivElement>(null);
  const toast = useToast();

  useEffect(() => {
    log.current?.scrollTo({ top: log.current.scrollHeight, behavior: "smooth" });
  }, [history.data?.length, proposal, busy]);

  const send = async (msg: string) => {
    if (!msg.trim() || busy) return;
    setBusy(true);
    setProposal(null);
    setBase(yaml);
    setText("");
    try {
      const res = await api.sendChat(name, msg, yaml);
      setProposal(res);
      history.reload();
    } catch (e) {
      toast("error", `Planner: ${(e as Error).message}`);
      history.reload();
    } finally {
      setBusy(false);
    }
  };

  const messages = history.data ?? [];
  return (
    <div className="chat">
      <div className="chat-log" ref={log}>
        {messages.length === 0 && !busy && (
          <div className="empty" style={{ padding: "24px 8px" }}>
            <Sparkles />
            <h3>Ask the planner to change this flow</h3>
            <p>It edits the YAML for you; you review the diff before anything changes.</p>
          </div>
        )}
        {messages.map((m) => (
          <Message key={m.id} m={m} />
        ))}
        {busy && (
          <div className="msg assistant">
            <div className="who">
              <Sparkles size={12} /> planner
            </div>
            <span className="row">
              <Spinner /> Revising the flow and validating it…
            </span>
          </div>
        )}
        {proposal && (
          <Proposal
            before={base}
            result={proposal}
            stale={base !== yaml}
            onApply={() => {
              onApply(proposal.yaml);
              setProposal(null);
              toast("ok", "Applied to the editor — save to keep it");
            }}
            onDiscard={() => setProposal(null)}
          />
        )}
      </div>
      <div className="chat-input">
        {messages.length < 3 && !busy && (
          <div className="suggestions">
            {SUGGESTIONS.map((s) => (
              <button key={s} className="suggestion" onClick={() => send(s)}>
                {s}
              </button>
            ))}
          </div>
        )}
        <textarea
          className="input"
          placeholder="e.g. add a lint check after the fix step"
          value={text}
          disabled={busy}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              send(text);
            }
          }}
        />
        <div className="row">
          <span className="small faint">Enter to send · Shift+Enter for a new line</span>
          <span className="spacer" />
          <button className="btn primary sm" disabled={busy || !text.trim()} onClick={() => send(text)}>
            <Send />
            Send
          </button>
        </div>
      </div>
    </div>
  );
}

function Message({ m }: { m: ChatMessage }) {
  if (m.role === "system") return <div className="msg system">{m.content}</div>;
  return (
    <div className={`msg ${m.role}`} title={timeAgo(m.created_at)}>
      {m.role === "assistant" && (
        <div className="who">
          <Sparkles size={12} /> planner
        </div>
      )}
      {m.content}
    </div>
  );
}

function Proposal({ before, result, stale, onApply, onDiscard }: { before: string; result: ChatResult; stale: boolean; onApply: () => void; onDiscard: () => void }) {
  const lines = useMemo(() => {
    const parts = diffLines(before, result.yaml);
    const out: { kind: "add" | "del" | "ctx" | "gap"; text: string }[] = [];
    parts.forEach((p, i) => {
      const ls = p.value.replace(/\n$/, "").split("\n");
      if (p.added) ls.forEach((t) => out.push({ kind: "add", text: "+ " + t }));
      else if (p.removed) ls.forEach((t) => out.push({ kind: "del", text: "- " + t }));
      else {
        const first = i === 0;
        const last = i === parts.length - 1;
        const keep = 2;
        if (ls.length <= keep * 2 + 1) ls.forEach((t) => out.push({ kind: "ctx", text: "  " + t }));
        else {
          if (!first) ls.slice(0, keep).forEach((t) => out.push({ kind: "ctx", text: "  " + t }));
          out.push({ kind: "gap", text: `  … ${ls.length - (first || last ? keep : keep * 2)} unchanged lines` });
          if (!last) ls.slice(-keep).forEach((t) => out.push({ kind: "ctx", text: "  " + t }));
        }
      }
    });
    return out;
  }, [before, result.yaml]);
  const changed = lines.some((l) => l.kind === "add" || l.kind === "del");
  const errors = result.issues.filter((i) => i.severity === "error").length;

  return (
    <div className="proposal">
      <div className="proposal-head">
        <Sparkles size={14} />
        Proposed change
        <span className="spacer" />
        {errors ? <span className="pill invalid">{errors} error{errors > 1 ? "s" : ""}</span> : <span className="pill valid">valid</span>}
      </div>
      {changed ? (
        <pre className="diff">
          {lines.map((l, i) => (
            <div key={i} className={l.kind}>
              {l.text}
            </div>
          ))}
        </pre>
      ) : (
        <div className="side-pad small muted">The planner made no changes.</div>
      )}
      {stale && <div className="warn-box" style={{ margin: "0 12px 8px" }}>You edited the flow since asking; applying replaces those edits.</div>}
      <div className="row" style={{ padding: "8px 12px 12px" }}>
        <button className="btn primary sm" disabled={!changed} onClick={onApply}>
          <Check />
          Apply to editor
        </button>
        <button className="btn ghost sm" onClick={onDiscard}>
          <X />
          Discard
        </button>
      </div>
    </div>
  );
}
