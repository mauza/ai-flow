// Package tmpl renders ${{ path.to.value ?? "fallback" }} references.
//
// Deliberately tiny: dotted paths into a map, an optional ?? fallback, nothing
// else. Rendering happens once, in the control plane, so model output that
// contains template syntax is never evaluated again.
package tmpl

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var refRe = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

type Ref struct {
	Raw      string   // full ${{ ... }} text
	Path     []string // e.g. nodes, fix, outputs, summary
	Fallback *string  // set when ?? was used
}

// Refs lists the references in s. Malformed references are returned as errors.
func Refs(s string) ([]Ref, []error) {
	var refs []Ref
	var errs []error
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		r, err := parse(m[0], m[1])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		refs = append(refs, r)
	}
	return refs, errs
}

var pathRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)*$`)

func parse(raw, expr string) (Ref, error) {
	r := Ref{Raw: raw}
	path := expr
	if i := strings.Index(expr, "??"); i >= 0 {
		path = strings.TrimSpace(expr[:i])
		lit := strings.TrimSpace(expr[i+2:])
		fb, err := strconv.Unquote(lit)
		if err != nil {
			return r, fmt.Errorf("%s: fallback must be a quoted string", raw)
		}
		r.Fallback = &fb
	}
	if !pathRe.MatchString(path) {
		return r, fmt.Errorf("%s: expected a dotted path like nodes.fix.outputs.summary", raw)
	}
	r.Path = strings.Split(path, ".")
	return r, nil
}

// Render replaces every reference with its value from ctx. Missing values
// without a fallback render as empty strings and are reported in missing.
func Render(s string, ctx map[string]any) (out string, missing []string) {
	out = refRe.ReplaceAllStringFunc(s, func(raw string) string {
		m := refRe.FindStringSubmatch(raw)
		r, err := parse(raw, m[1])
		if err != nil {
			missing = append(missing, raw)
			return ""
		}
		v, ok := Lookup(ctx, r.Path)
		if !ok || v == nil {
			if r.Fallback != nil {
				return *r.Fallback
			}
			missing = append(missing, strings.Join(r.Path, "."))
			return ""
		}
		return Stringify(v)
	})
	return out, missing
}

// Lookup walks a dotted path through nested maps.
func Lookup(ctx map[string]any, path []string) (any, bool) {
	var cur any = ctx
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Stringify renders scalars plainly and everything else as indented JSON.
func Stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool, int, int64, float64, json.Number:
		return fmt.Sprint(t)
	default:
		b, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}
