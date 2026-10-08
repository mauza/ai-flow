import { useMemo } from "react";
import { diffLines } from "diff";

/** A unified line diff with unchanged runs collapsed. */
export default function DiffView({ before, after }: { before: string; after: string }) {
  const lines = useMemo(() => {
    const parts = diffLines(before, after);
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
  }, [before, after]);
  return (
    <div className="diff">
      {lines.map((l, i) => (
        <div key={i} className={l.kind}>
          {l.text}
        </div>
      ))}
    </div>
  );
}
