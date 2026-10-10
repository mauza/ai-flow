package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

type executionSnapshot struct {
	Version  int               `json:"version"`
	Resolved *resolve.Resolved `json:"resolved"`
	Images   map[string]string `json:"images"`
	// Consumers outside the engine still read these catalog/environment settings.
	// Refuse drift rather than silently executing a different model/repo/skill.
	// Hashing also avoids copying URLs or MCP configuration into the database.
	Dependencies string `json:"dependencies"`
}

func (e *Engine) snapshot(res *resolve.Resolved) ([]byte, error) {
	images := map[string]string{}
	for _, n := range res.Nodes {
		images[n.Runtime] = e.cfg.Current().Catalog.Runtimes[n.Runtime].Image
	}
	deps, err := e.executionDependencies(res)
	if err != nil {
		return nil, err
	}
	return json.Marshal(executionSnapshot{Version: 1, Resolved: res, Images: images, Dependencies: deps})
}

func (e *Engine) executionDependencies(res *resolve.Resolved) (string, error) {
	return executionDependencies(res, e.cfg.Current())
}

// executionDependencies hashes the live settings a run depends on beyond its
// snapshot. Only what changes execution counts: descriptions, notes and other
// planner hints can be edited in Settings without failing active runs.
func executionDependencies(res *resolve.Resolved, cfg *config.Config) (string, error) {
	deps := map[string]any{}
	addGrant := func(name string) {
		g := cfg.Catalog.Grants[name]
		if g == nil {
			deps["grant/"+name] = nil
			return
		}
		pinned := *g
		pinned.Description = ""
		deps["grant/"+name] = pinned
		if g.Kind == config.GrantMCP {
			// Headers and their secret values remain current authorization.
			deps["mcp/"+g.Server] = cfg.Env.MCP.Servers[g.Server].URL
		}
	}
	addGrant(res.Repo)
	for _, n := range res.Nodes {
		for _, g := range n.Grants {
			name, _, _ := strings.Cut(g, ":")
			addGrant(name)
		}
		for _, s := range n.Skills {
			if sk := cfg.Catalog.Skills[s]; sk != nil {
				deps["skill/"+s] = sk.Files
			} else {
				deps["skill/"+s] = nil
			}
		}
		if h := cfg.Catalog.Harnesses[n.Harness]; n.Type == flow.TypeAgent && (h.Instructions != "" || len(h.Settings) > 0) {
			// Only when set, so harnesses without either keep earlier hashes.
			deps["harness/"+n.Harness] = config.Harness{Instructions: h.Instructions, Settings: h.Settings}
		}
		if n.LLM != nil {
			for _, name := range append([]string{n.LLM.Model}, n.LLM.Fallbacks...) {
				m := cfg.Catalog.Models[name]
				if m == nil {
					deps["model/"+name] = nil
					continue
				}
				deps["model/"+name] = pinnedModel(m)
				deps["upstream/"+m.Upstream] = cfg.Env.LLM.Upstreams[m.Upstream].BaseURL
			}
		}
	}
	runs := cfg.Env.Runs
	runs.MaxConcurrent = 0           // admission control is intentionally current
	runs.DefaultTimeout.Duration = 0 // already resolved per node
	deps["runs"] = runs
	deps["git_author"] = []string{cfg.Env.Git.AuthorName, cfg.Env.Git.AuthorEmail}
	deps["github_api"] = cfg.Env.GitHub.APIURL
	data, err := json.Marshal(deps)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// pinnedModel keeps the model fields that change what a run executes: the
// upstream and model id, what the runner tells the harness (context size,
// reasoning, thinking format, reply cap), and the model's own llm defaults.
// Planner hints (size, tool use, cost label, notes), prices (accounting) and
// max_concurrency (admission control) may change while runs are active.
func pinnedModel(m *config.Model) config.Model {
	return config.Model{Upstream: m.Upstream, Model: m.Model, ContextTokens: m.ContextTokens, Reasoning: m.Reasoning,
		ThinkingFormat: m.ThinkingFormat, MaxOutputTokens: m.MaxOutputTokens, LLM: m.LLM}
}

// DriftedRuns lists the queued, running and waiting runs that cfg would fail
// with configuration drift.
func (e *Engine) DriftedRuns(ctx context.Context, cfg *config.Config) ([]string, error) {
	runs, err := e.store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.RunQueued, store.RunRunning, store.RunWaiting}, Limit: -1})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range runs {
		snap, err := e.loadSnapshot(ctx, r)
		if err != nil {
			continue // fails on its own; not caused by this change
		}
		deps, err := executionDependencies(snap.Resolved, cfg)
		if err != nil {
			return nil, err
		}
		if deps != snap.Dependencies {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

func (e *Engine) loadSnapshot(ctx context.Context, r *store.Run) (*executionSnapshot, error) {
	data := r.Snapshot
	if len(data) == 0 {
		// Legacy runs cannot recover historical settings. Pin the first observed
		// configuration once, durably, and record that adoption for operators.
		fresh, err := e.store.GetRun(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		data = fresh.Snapshot
		if len(data) == 0 {
			fv, err := e.store.GetFlow(ctx, r.FlowName, r.FlowVersion)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil, rejectf("saved flow %s v%d is missing", r.FlowName, r.FlowVersion)
				}
				return nil, err
			}
			res, err := e.resolve(fv)
			if err != nil {
				return nil, err
			}
			// The branch base was already pinned on the run even in version 0.
			res.Base = r.Base
			data, err = e.snapshot(res)
			if err != nil {
				return nil, err
			}
			if err := e.store.PinSnapshot(ctx, r.ID, data); err != nil {
				return nil, err
			}
			fresh, err = e.store.GetRun(ctx, r.ID)
			if err != nil {
				return nil, err
			}
			data = fresh.Snapshot
			e.event(ctx, r, "", "snapshot_adopted", "Legacy run execution settings pinned from current configuration")
		}
	}
	var snap executionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, rejectf("execution snapshot: %v", err)
	}
	if snap.Version != 1 || snap.Resolved == nil || snap.Resolved.Flow == nil || len(snap.Resolved.Nodes) == 0 {
		return nil, rejectf("unsupported or invalid execution snapshot version %d", snap.Version)
	}
	return &snap, nil
}

// Resolved preserves the broker-facing API. Defaults/presets/budgets are pinned;
// authorization is always checked against the current project and catalog.
func (e *Engine) Resolved(ctx context.Context, r *store.Run) (*resolve.Resolved, error) {
	snap, err := e.loadSnapshot(ctx, r)
	if err != nil {
		return nil, err
	}
	res := snap.Resolved
	current := *res
	current.Project = e.cfg.Current().Projects[res.Flow.Metadata.Project]
	if issues := resolve.Validate(&current, e.cfg.Current()); resolve.HasErrors(issues) {
		return nil, rejectf("pinned execution is not currently authorized/valid: %v", issues)
	}
	deps, err := e.executionDependencies(res)
	if err != nil {
		return nil, err
	}
	if deps != snap.Dependencies {
		return nil, rejectf("execution configuration drift for run %s; create a new run with the intended settings", r.ID)
	}
	return res, nil
}
