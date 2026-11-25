package resolve

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
)

// expandTemplates rewrites any node with `uses: template/<name>` into the
// template's concrete nodes prefixed by the outer node's ID. Targets using
// `$out.<outcome>` resolve to the outer node's `next.<outcome>`. Unknown
// templates are left unexpanded so Validate reports a clear error on `uses`.
func expandTemplates(f *flow.Flow, cfg *config.Config) *flow.Flow {
	hasTpl := false
	for _, n := range f.Spec.Nodes {
		if n != nil && strings.HasPrefix(n.Uses, "template/") {
			name := strings.TrimPrefix(n.Uses, "template/")
			if cfg.Catalog.Templates[name] != nil {
				hasTpl = true
				break
			}
		}
	}
	if !hasTpl {
		return f
	}
	out := *f
	out.Spec = f.Spec
	out.Spec.Nodes = make(map[string]*flow.Node, len(f.Spec.Nodes))
	entryMap := map[string]string{} // outer node ID -> expanded start node ID

	for _, outerID := range slices.Sorted(maps.Keys(f.Spec.Nodes)) {
		raw := f.Spec.Nodes[outerID]
		if raw == nil || !strings.HasPrefix(raw.Uses, "template/") {
			out.Spec.Nodes[outerID] = cloneNode(raw)
			continue
		}
		tplName := strings.TrimPrefix(raw.Uses, "template/")
		tpl := cfg.Catalog.Templates[tplName]
		if tpl == nil {
			out.Spec.Nodes[outerID] = cloneNode(raw)
			continue
		}
		// Build deterministic internal -> expanded node ID mapping.
		idMap := map[string]string{}
		for _, subID := range slices.Sorted(maps.Keys(tpl.Nodes)) {
			idMap[subID] = expandedNodeID(outerID, subID)
		}
		entryMap[outerID] = idMap[tpl.Start]

		for _, subID := range slices.Sorted(maps.Keys(tpl.Nodes)) {
			sub := cloneNode(tpl.Nodes[subID])
			// Apply outer caller overrides/defaults to inner nodes cleanly.
			if raw.Model != "" && sub.Model == "" && (sub.Type == flow.TypeLLM || sub.Type == flow.TypeAgent || sub.Uses != "") {
				if p := presetFor(sub, cfg); sub.Type == flow.TypeLLM || sub.Type == flow.TypeAgent || (p != nil && (p.Type == flow.TypeLLM || p.Type == flow.TypeAgent)) {
					sub.Model = raw.Model
				}
			}
			if raw.Runtime != "" && sub.Runtime == "" {
				if p := presetFor(sub, cfg); flow.PodType(sub.Type) || (p != nil && flow.PodType(p.Type)) {
					sub.Runtime = raw.Runtime
				}
			}
			if raw.Harness != "" && sub.Harness == "" {
				if p := presetFor(sub, cfg); sub.Type == flow.TypeAgent || (p != nil && p.Type == flow.TypeAgent) {
					sub.Harness = raw.Harness
				}
			}
			if len(raw.Grants) > 0 && len(sub.Grants) == 0 {
				if p := presetFor(sub, cfg); flow.PodType(sub.Type) || (p != nil && flow.PodType(p.Type)) {
					sub.Grants = append([]string(nil), raw.Grants...)
				}
			}
			// Remap transitions ($out.<name> -> outer.Next[name], inner -> expanded).
			if len(sub.Next) > 0 {
				next := make(map[string]string, len(sub.Next))
				for outcome, target := range sub.Next {
					next[outcome] = remapTarget(target, idMap, raw.Next)
				}
				sub.Next = next
			}
			if sub.OnExhausted != "" {
				sub.OnExhausted = remapTarget(sub.OnExhausted, idMap, raw.Next)
			}
			if len(sub.Branches) > 0 {
				branches := make([]string, len(sub.Branches))
				for i, b := range sub.Branches {
					branches[i] = remapTarget(b, idMap, raw.Next)
				}
				sub.Branches = branches
			}
			if sub.Join != "" {
				sub.Join = remapTarget(sub.Join, idMap, raw.Next)
			}
			// Remap template references `nodes.<subID>.` inside prompt/inputs/with/cases.
			sub.Prompt = remapNodeRefs(sub.Prompt, idMap)
			if len(sub.Inputs) > 0 {
				in := make(map[string]string, len(sub.Inputs))
				for k, v := range sub.Inputs {
					in[k] = remapNodeRefs(v, idMap)
				}
				sub.Inputs = in
			}
			if len(sub.With) > 0 {
				w := make(map[string]any, len(sub.With))
				for k, v := range sub.With {
					if s, ok := v.(string); ok {
						w[k] = remapNodeRefs(s, idMap)
					} else {
						w[k] = v
					}
				}
				sub.With = w
			}
			if len(sub.Cases) > 0 {
				cases := make([]flow.Case, len(sub.Cases))
				for i, cs := range sub.Cases {
					cases[i] = flow.Case{When: remapNodeRefs(cs.When, idMap), Outcome: cs.Outcome}
				}
				sub.Cases = cases
			}
			out.Spec.Nodes[idMap[subID]] = sub
		}
	}

	// Remap outer references pointing at any expanded template node to its entry node.
	if target, ok := entryMap[out.Spec.Start]; ok {
		out.Spec.Start = target
	}
	for _, n := range out.Spec.Nodes {
		if len(n.Next) > 0 {
			next := make(map[string]string, len(n.Next))
			for o, t := range n.Next {
				if mapped, ok := entryMap[t]; ok {
					t = mapped
				}
				next[o] = t
			}
			n.Next = next
		}
		if mapped, ok := entryMap[n.OnExhausted]; ok {
			n.OnExhausted = mapped
		}
		for i, b := range n.Branches {
			if mapped, ok := entryMap[b]; ok {
				n.Branches[i] = mapped
			}
		}
		if mapped, ok := entryMap[n.Join]; ok {
			n.Join = mapped
		}
	}
	return &out
}

func presetFor(n *flow.Node, cfg *config.Config) *config.Preset {
	if n == nil || !strings.HasPrefix(n.Uses, "preset/") {
		return nil
	}
	return cfg.Catalog.Presets[strings.TrimPrefix(n.Uses, "preset/")]
}

func remapTarget(target string, idMap, outerNext map[string]string) string {
	if outName, ok := strings.CutPrefix(target, "$out."); ok {
		if t, has := outerNext[outName]; has {
			return t
		}
		return flow.Fail
	}
	if mapped, ok := idMap[target]; ok {
		return mapped
	}
	return target
}

func remapNodeRefs(s string, idMap map[string]string) string {
	if s == "" || !strings.Contains(s, "nodes.") {
		return s
	}
	for subID, expanded := range idMap {
		s = strings.ReplaceAll(s, "nodes."+subID+".", "nodes."+expanded+".")
	}
	return s
}

func expandedNodeID(outerID, subID string) string {
	full := outerID + "_" + subID
	if len(full) <= 40 {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	prefix := full[:33]
	return fmt.Sprintf("%s_%x", strings.TrimRight(prefix, "_"), sum[:3])
}
