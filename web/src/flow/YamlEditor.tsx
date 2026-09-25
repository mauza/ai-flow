import { useEffect, useMemo, useState } from "react";
import CodeMirror, { EditorView } from "@uiw/react-codemirror";
import { yaml as yamlLang } from "@codemirror/lang-yaml";
import { linter, lintGutter, type Diagnostic } from "@codemirror/lint";
import { Check, Copy, Download } from "lucide-react";
import type { Issue } from "../api";

function useDark(): boolean {
  const q = "(prefers-color-scheme: dark)";
  const [dark, setDark] = useState(() => window.matchMedia(q).matches);
  useEffect(() => {
    const m = window.matchMedia(q);
    const on = () => setDark(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return dark;
}

/** Find the line range for an issue: the field inside the node's block, else the node key. */
function locate(text: string, issue: Issue): { from: number; to: number } | null {
  const lines = text.split("\n");
  const offsets: number[] = [];
  let pos = 0;
  for (const l of lines) {
    offsets.push(pos);
    pos += l.length + 1;
  }
  const lineRange = (i: number) => ({ from: offsets[i], to: offsets[i] + lines[i].length });
  const field = issue.field?.split(/[.[]/)[0];
  if (!issue.node) {
    if (field) {
      const leaf = issue.field!.split(".").pop()!;
      const i = lines.findIndex((l) => new RegExp(`^\\s*${leaf}:`).test(l));
      if (i >= 0) return lineRange(i);
    }
    return lines.length ? lineRange(0) : null;
  }
  const start = lines.findIndex((l) => new RegExp(`^\\s+${issue.node}:\\s*$`).test(l));
  if (start < 0) return null;
  const indent = lines[start].search(/\S/);
  if (field) {
    for (let i = start + 1; i < lines.length; i++) {
      const ind = lines[i].search(/\S/);
      if (lines[i].trim() && ind <= indent) break;
      if (new RegExp(`^\\s+${field}:`).test(lines[i])) return lineRange(i);
    }
  }
  return lineRange(start);
}

export default function YamlEditor({ value, onChange, issues, readOnly, fileName }: { value: string; onChange: (v: string) => void; issues: Issue[]; readOnly?: boolean; fileName?: string }) {
  const [copied, setCopied] = useState(false);
  const dark = useDark();
  const extensions = useMemo(
    () => [
      yamlLang(),
      lintGutter(),
      linter(
        (view) => {
          const text = view.state.doc.toString();
          const out: Diagnostic[] = [];
          for (const i of issues) {
            const r = locate(text, i);
            if (!r) continue;
            out.push({ from: r.from, to: r.to, severity: i.severity === "error" ? "error" : "warning", message: `${i.node ? i.node + (i.field ? "." + i.field : "") + ": " : ""}${i.message}` });
          }
          return out;
        },
        { delay: 100 },
      ),
      EditorView.lineWrapping,
    ],
    [issues],
  );
  const download = () => {
    const url = URL.createObjectURL(new Blob([value], { type: "application/yaml" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `${fileName ?? "flow"}.yaml`;
    a.click();
    URL.revokeObjectURL(url);
  };
  return (
    <div className="cm-wrap">
      <div className="row" style={{ padding: "6px 10px", borderBottom: "1px solid var(--border)", gap: 6 }}>
        <span className="small faint">Edits here update the graph as you type. Ctrl+S saves a version.</span>
        <span className="spacer" />
        <button
          className="btn ghost sm"
          onClick={() => navigator.clipboard?.writeText(value).then(() => (setCopied(true), window.setTimeout(() => setCopied(false), 1500)))}
        >
          {copied ? <Check /> : <Copy />}
          {copied ? "Copied" : "Copy"}
        </button>
        <button className="btn ghost sm" onClick={download}>
          <Download />
          Download
        </button>
      </div>
      <div style={{ flex: 1, minHeight: 0 }}>
      <CodeMirror
        value={value}
        onChange={onChange}
        extensions={extensions}
        theme={dark ? "dark" : "light"}
        readOnly={readOnly}
        height="100%"
        basicSetup={{ foldGutter: true, highlightActiveLine: true, tabSize: 2 }}
      />
      </div>
    </div>
  );
}
