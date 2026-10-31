package tmpl

import "testing"

func TestFallbacks(t *testing.T) {
	ctx := map[string]any{"nodes": map[string]any{"ci": map[string]any{"outputs": map[string]any{"failed": []any{"lint"}}}}}
	for in, want := range map[string]string{
		`${{ nodes.ci.outputs.failed ?? [] }}`:     "[\n  \"lint\"\n]",
		`${{ nodes.fix.outputs.failed ?? [] }}`:    "[]",
		`${{ nodes.fix.outputs.count ?? 0 }}`:      "0",
		`${{ nodes.fix.outputs.note ?? "none" }}`:  "none",
		`${{ nodes.fix.outputs.ok ?? false }}`:     "false",
		`${{ nodes.fix.outputs.map ?? {"a": 1} }}`: `{"a": 1}`,
	} {
		if _, errs := Refs(in); len(errs) != 0 {
			t.Errorf("%s: %v", in, errs)
		}
		if got, missing := Render(in, ctx); got != want || len(missing) != 0 {
			t.Errorf("%s = %q (missing %v), want %q", in, got, missing, want)
		}
	}
	for _, bad := range []string{`${{ a ?? none }}`, `${{ a ?? }}`, `${{ a ?? null }}`} {
		if _, errs := Refs(bad); len(errs) == 0 {
			t.Errorf("%s: want an error", bad)
		}
	}
}
