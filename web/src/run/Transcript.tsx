import { useEffect, useState } from "react";
import { api } from "../api";
import { Spinner } from "../ui";

type Item =
  | { kind: "msg"; role: string; text: string }
  | { kind: "tool"; name: string; args: string; result: string; error: boolean }
  | { kind: "log"; text: string };

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((c) => (c && typeof c === "object" && "text" in c ? String((c as { text: unknown }).text ?? "") : ""))
      .filter(Boolean)
      .join("\n");
  }
  return "";
}

function argSummary(name: string, args: Record<string, unknown>): string {
  const pick = (...keys: string[]) => keys.map((k) => args?.[k]).find((v) => typeof v === "string" && v) as string | undefined;
  switch (name) {
    case "bash":
      return pick("command") ?? "";
    case "read":
    case "write":
    case "edit":
      return pick("path", "file_path") ?? "";
    case "flow_finish":
      return `${args?.outcome ?? ""} — ${args?.summary ?? ""}`;
  }
  return JSON.stringify(args ?? {});
}

/** Parse pi JSON events, llm-node request/response lines, or plain check output. */
export function parseTranscript(raw: string): Item[] {
  const lines = raw.split("\n").filter((l) => l.trim());
  const parsed = lines.map((l) => {
    try {
      return JSON.parse(l) as Record<string, unknown>;
    } catch {
      return null;
    }
  });
  if (parsed.filter(Boolean).length < lines.length / 2) return [{ kind: "log", text: raw }];

  const items: Item[] = [];
  const tools = new Map<string, Extract<Item, { kind: "tool" }>>();
  for (const ev of parsed) {
    if (!ev) continue;
    switch (ev.type) {
      case "message_end": {
        const m = ev.message as { role?: string; content?: unknown } | undefined;
        if (!m || m.role === "toolResult") break;
        const text = textOf(m.content);
        if (text.trim()) items.push({ kind: "msg", role: m.role ?? "assistant", text });
        break;
      }
      case "tool_execution_start": {
        const it: Extract<Item, { kind: "tool" }> = {
          kind: "tool",
          name: String(ev.toolName ?? "tool"),
          args: argSummary(String(ev.toolName), (ev.args ?? {}) as Record<string, unknown>),
          result: "",
          error: false,
        };
        tools.set(String(ev.toolCallId), it);
        items.push(it);
        break;
      }
      case "tool_execution_end": {
        const it = tools.get(String(ev.toolCallId));
        if (it) {
          const r = ev.result as { content?: unknown } | undefined;
          it.result = textOf(r?.content) || (typeof ev.result === "string" ? ev.result : "");
          it.error = !!ev.isError;
        }
        break;
      }
      case "llm_request": {
        const msgs = (ev.messages ?? []) as { role: string; content: string }[];
        for (const m of msgs) items.push({ kind: "msg", role: m.role, text: m.content });
        break;
      }
      case "llm_response":
        items.push({ kind: "msg", role: "assistant", text: String(ev.content ?? "") });
        break;
    }
  }
  return items;
}

export default function Transcript({ runId, seq, live }: { runId: string; seq: number; live?: boolean }) {
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<string>();
  useEffect(() => {
    setItems(null);
    setError(undefined);
    let stop = false;
    const load = () =>
      api
        .transcript(runId, seq)
        .then((raw) => !stop && (setItems(parseTranscript(raw)), setError(undefined)))
        .catch((e) => !stop && setError(e.status === 404 ? (live ? "The agent is starting; its transcript appears here every few seconds." : "No transcript for this step.") : e.message));
    load();
    // running steps upload their transcript periodically; follow along
    const t = live ? window.setInterval(load, 5000) : undefined;
    return () => {
      stop = true;
      window.clearInterval(t);
    };
  }, [runId, seq, live]);

  if (error) return <div className="muted small">{error}</div>;
  if (!items) return <Spinner />;
  if (!items.length) return <div className="muted small">Empty transcript.</div>;
  return (
    <div className="tx">
      {items.map((it, i) => {
        if (it.kind === "log") return <pre key={i} className="code">{it.text}</pre>;
        if (it.kind === "msg")
          return (
            <div key={i} className={`tx-msg ${it.role === "user" ? "user" : "assistant"}`}>
              <div className="who">{it.role}</div>
              {it.text.length > 6000 ? it.text.slice(0, 6000) + "\n… (truncated)" : it.text}
            </div>
          );
        return (
          <details key={i} className={`tx-tool${it.error ? " err" : ""}`}>
            <summary>
              <span className="name">{it.name}</span>
              <span className="arg">{it.args}</span>
            </summary>
            {it.result && <pre className="code">{it.result.length > 8000 ? it.result.slice(0, 8000) + "\n… (truncated)" : it.result}</pre>}
          </details>
        );
      })}
    </div>
  );
}
