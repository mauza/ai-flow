import { diffLines } from "diff";

export interface DiffRow {
  kind: "add" | "del" | "ctx" | "gap";
  text: string;
}

/** A unified line diff with long unchanged runs collapsed to two lines of context. */
export function diffRows(before: string, after: string): DiffRow[] {
  const parts = diffLines(before, after);
  const out: DiffRow[] = [];
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
}

export default function DiffView({ rows }: { rows: DiffRow[] }) {
  return (
    <pre className="diff">
      {rows.map((l, i) => (
        <div key={i} className={l.kind}>
          {l.text}
        </div>
      ))}
    </pre>
  );
}
