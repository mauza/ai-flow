package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

// The catalog and projects live in the database (kv) once the server has
// started: the first start seeds them from the config files, and the UI edits
// them from then on. The environment always comes from the files.
const (
	kvCatalog  = "config/catalog"
	kvProject  = "config/project/"
	kvSeededAt = "config/seeded_at"
)

// CatalogSections are the catalog's named collections the UI edits entry by entry.
var CatalogSections = []string{"models", "presets", "runtimes", "harnesses", "grants", "skills"}

// CatalogSettings are the catalog's single-value settings.
var CatalogSettings = []string{"planner", "defaults"}

// LoadStoredConfig returns the config to serve: the files' environment with
// the catalog and projects stored in the database. On the first start it
// seeds the database from files and returns files unchanged.
func LoadStoredConfig(ctx context.Context, st *store.Store, files *config.Config) (cfg *config.Config, seeded bool, err error) {
	cat, err := st.GetKV(ctx, kvCatalog)
	if errors.Is(err, store.ErrNotFound) {
		c, projects := files.Docs()
		set := map[string]string{kvCatalog: string(c), kvSeededAt: strconv.FormatInt(store.Now(), 10)}
		for name, d := range projects {
			set[kvProject+name] = string(d)
		}
		if err := st.WriteKV(ctx, set, nil); err != nil {
			return nil, false, err
		}
		return files, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	stored, err := st.ListKV(ctx, kvProject)
	if err != nil {
		return nil, false, err
	}
	var docs [][]byte
	for _, d := range stored {
		docs = append(docs, []byte(d))
	}
	next, err := files.WithDocs([]byte(cat), docs)
	if err != nil {
		return nil, false, fmt.Errorf("stored catalog and projects (edit them in the UI or clear the config/ keys to reseed from files): %w", err)
	}
	files.Publish(next)
	return next, false, nil
}

// SeededAt is when the database took over the catalog and projects (unix ms),
// or 0 if unknown.
func (a *App) SeededAt(ctx context.Context) int64 {
	v, _ := a.Store.GetKV(ctx, kvSeededAt)
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// ConfigEdit changes one catalog entry, catalog setting or project. Section is
// one of CatalogSections, "settings" (Name is one of CatalogSettings) or
// "projects". An empty YAML deletes the entry.
type ConfigEdit struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	YAML    string `json:"yaml"`
}

// EditConfig applies edits together: they are validated as a whole against
// the environment, stored, and published to every component at once. The
// warnings name saved flows that the change breaks.
func (a *App) EditConfig(ctx context.Context, edits ...ConfigEdit) (warnings []string, err error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	cur := a.Cfg.Current()
	catDoc, projDocs := cur.Docs()
	var cat map[string]any
	if err := yaml.Unmarshal(catDoc, &cat); err != nil {
		return nil, err
	}
	if cat == nil {
		cat = map[string]any{}
	}
	touched := map[string]bool{}
	for _, e := range edits {
		name := strings.TrimSpace(e.Name)
		if name == "" || strings.ContainsAny(name, " \t\n") {
			return nil, fmt.Errorf("%s: a name without spaces is required", e.Section)
		}
		var value any
		if strings.TrimSpace(e.YAML) != "" {
			if err := yaml.Unmarshal([]byte(e.YAML), &value); err != nil {
				return nil, fmt.Errorf("%s %s: %w", e.Section, name, err)
			}
			if _, ok := value.(map[string]any); !ok {
				return nil, fmt.Errorf("%s %s: expected a YAML mapping", e.Section, name)
			}
		}
		switch {
		case slices.Contains(CatalogSections, e.Section):
			section, _ := cat[e.Section].(map[string]any)
			if section == nil {
				section = map[string]any{}
				cat[e.Section] = section
			}
			if value == nil {
				if _, ok := section[name]; !ok {
					return nil, fmt.Errorf("%s %s does not exist", e.Section, name)
				}
				delete(section, name)
			} else {
				section[name] = value
			}
		case e.Section == "settings" && slices.Contains(CatalogSettings, name):
			if value == nil {
				delete(cat, name)
			} else {
				cat[name] = value
			}
		case e.Section == "projects":
			touched[name] = true
			if value == nil {
				if _, ok := projDocs[name]; !ok {
					return nil, fmt.Errorf("project %s does not exist", name)
				}
				delete(projDocs, name)
				continue
			}
			doc := value.(map[string]any)
			doc["apiVersion"], doc["kind"] = "ai-flow/v1alpha1", "Project"
			doc["metadata"] = map[string]any{"name": name}
			b, err := yaml.Marshal(doc)
			if err != nil {
				return nil, err
			}
			projDocs[name] = b
		default:
			return nil, fmt.Errorf("unknown config section %q", e.Section)
		}
	}
	cat["apiVersion"], cat["kind"] = "ai-flow/v1alpha1", "Catalog"
	newCat, err := yaml.Marshal(cat)
	if err != nil {
		return nil, err
	}
	var docs [][]byte
	for _, d := range projDocs {
		docs = append(docs, d)
	}
	next, err := cur.WithDocs(newCat, docs)
	if err != nil {
		return nil, err
	}
	set := map[string]string{kvCatalog: string(newCat)}
	var del []string
	for name := range touched {
		if d, ok := projDocs[name]; ok {
			set[kvProject+name] = string(d)
		} else {
			del = append(del, kvProject+name)
		}
	}
	if err := a.Store.WriteKV(ctx, set, del); err != nil {
		return nil, err
	}
	warnings = a.brokenFlows(ctx, cur, next)
	a.Cfg.Publish(next)
	a.Hub.Publish(hub.Event{Type: "config", ID: time.Now().Format(time.RFC3339Nano)})
	return warnings, nil
}

// brokenFlows lists saved flows (latest versions) that validated before the
// change and have errors after it.
func (a *App) brokenFlows(ctx context.Context, before, after *config.Config) []string {
	flows, err := a.Store.ListFlows(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, fv := range flows {
		f, err := flow.Parse([]byte(fv.YAML))
		if err != nil {
			continue
		}
		if resolve.HasErrors(resolve.Validate(resolve.Resolve(f, before), before)) {
			continue
		}
		issues := resolve.Validate(resolve.Resolve(f, after), after)
		if resolve.HasErrors(issues) {
			for _, i := range issues {
				if i.Severity == resolve.Error {
					out = append(out, fmt.Sprintf("flow %s: %s", fv.Name, i.String()))
					break
				}
			}
		}
	}
	return out
}
