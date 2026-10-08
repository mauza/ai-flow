package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
//
// The config files still matter after that: a release bumps runtime images
// there, and sync-catalog brings in new presets. So the files as last read are
// kept too, and on each start every entry the files changed since then (a
// model, preset, runtime, harness, grant, skill, planner setting or project)
// replaces the database's version of that entry. Entries the files did not
// change keep their UI edits.
const (
	kvCatalog      = "config/catalog"
	kvProject      = "config/project/"
	kvSeededAt     = "config/seeded_at"
	kvFilesCatalog = "config/files/catalog"
	kvFilesProject = "config/files/project/"
	kvFilesError   = "config/files/error"
)

// CatalogSections are the catalog's named collections the UI edits entry by entry.
var CatalogSections = []string{"models", "presets", "runtimes", "harnesses", "grants", "skills"}

// CatalogSettings are the catalog's single-value settings.
var CatalogSettings = []string{"planner", "defaults"}

// StoredConfig says how LoadStoredConfig arrived at the config it returned.
type StoredConfig struct {
	Seeded    bool     // first start: the database was filled from the files
	FromFiles []string // entries the files changed since the last start, now in the database
	Error     string   // the file changes could not be applied; the database version is served
}

// LoadStoredConfig returns the config to serve: the files' environment with
// the catalog and projects stored in the database, after taking in what
// changed in the files since the last start.
func LoadStoredConfig(ctx context.Context, st *store.Store, files *config.Config) (*config.Config, *StoredConfig, error) {
	fileCat, fileProjects := files.Docs()
	baseline := map[string]string{kvFilesCatalog: string(fileCat)}
	for name, d := range fileProjects {
		baseline[kvFilesProject+name] = string(d)
	}
	cat, err := st.GetKV(ctx, kvCatalog)
	if errors.Is(err, store.ErrNotFound) {
		set := map[string]string{kvCatalog: string(fileCat), kvSeededAt: strconv.FormatInt(store.Now(), 10)}
		for name, d := range fileProjects {
			set[kvProject+name] = string(d)
		}
		maps.Copy(set, baseline)
		if err := st.WriteKV(ctx, set, nil); err != nil {
			return nil, nil, err
		}
		return files, &StoredConfig{Seeded: true}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	projects, err := st.ListKV(ctx, kvProject)
	if err != nil {
		return nil, nil, err
	}
	dbProjects := map[string]string{}
	for k, v := range projects {
		dbProjects[strings.TrimPrefix(k, kvProject)] = v
	}
	report := &StoredConfig{}
	prevCat, err := st.GetKV(ctx, kvFilesCatalog)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No record of the files yet: start tracking them from here.
		if err := st.WriteKV(ctx, baseline, nil); err != nil {
			return nil, nil, err
		}
	case err != nil:
		return nil, nil, err
	default:
		prevProjects, err := st.ListKV(ctx, kvFilesProject)
		if err != nil {
			return nil, nil, err
		}
		prev := map[string]string{}
		for k, v := range prevProjects {
			prev[strings.TrimPrefix(k, kvFilesProject)] = v
		}
		merged, mergedProjects, changed, err := mergeFileChanges(cat, dbProjects, prevCat, prev, string(fileCat), fileProjects)
		if err == nil && len(changed) > 0 {
			_, err = files.WithDocs([]byte(merged), docList(mergedProjects))
		}
		if err != nil {
			// Keep serving the database version, and retry on the next start.
			report.Error = fmt.Sprintf("changes in the config files (%s) were not applied: %v", strings.Join(changed, ", "), err)
			st.SetKV(ctx, kvFilesError, report.Error)
			break
		}
		set := maps.Clone(baseline)
		var del []string
		if len(changed) > 0 {
			set[kvCatalog] = merged
			for name := range dbProjects {
				if _, ok := mergedProjects[name]; !ok {
					del = append(del, kvProject+name)
				}
			}
			for name, d := range mergedProjects {
				set[kvProject+name] = d
			}
			cat, dbProjects = merged, mergedProjects
			report.FromFiles = changed
		}
		for name := range prev {
			if _, ok := fileProjects[name]; !ok {
				del = append(del, kvFilesProject+name)
			}
		}
		del = append(del, kvFilesError)
		if err := st.WriteKV(ctx, set, del); err != nil {
			return nil, nil, err
		}
	}
	next, err := files.WithDocs([]byte(cat), docList(dbProjects))
	if err != nil {
		return nil, nil, fmt.Errorf("stored catalog and projects (edit them in the UI or clear the config/ keys to reseed from files): %w", err)
	}
	files.Publish(next)
	return next, report, nil
}

// mergeFileChanges applies to the database's documents every entry the files
// changed between prev and now. It returns the merged documents and the
// changed entries ("models gpt-6-luna", "projects web-games").
func mergeFileChanges(dbCat string, dbProjects map[string]string, prevCat string, prevProjects map[string]string, fileCat string, fileProjects map[string][]byte) (string, map[string]string, []string, error) {
	var db, prev, now map[string]any
	for _, x := range []struct {
		src string
		dst *map[string]any
	}{{dbCat, &db}, {prevCat, &prev}, {fileCat, &now}} {
		if err := yaml.Unmarshal([]byte(x.src), x.dst); err != nil {
			return "", nil, nil, err
		}
		if *x.dst == nil {
			*x.dst = map[string]any{}
		}
	}
	var changed []string
	for _, section := range CatalogSections {
		d, _ := db[section].(map[string]any)
		if d == nil {
			d = map[string]any{}
		}
		p, _ := prev[section].(map[string]any)
		n, _ := now[section].(map[string]any)
		for _, name := range unionKeys(p, n) {
			pv, inPrev := p[name]
			nv, inNow := n[name]
			if inPrev == inNow && sameJSON(pv, nv) {
				continue
			}
			changed = append(changed, section+" "+name)
			if inNow {
				d[name] = nv
			} else {
				delete(d, name)
			}
		}
		if len(d) > 0 {
			db[section] = d
		}
	}
	for _, key := range CatalogSettings {
		pv, inPrev := prev[key]
		nv, inNow := now[key]
		if inPrev == inNow && sameJSON(pv, nv) {
			continue
		}
		changed = append(changed, "settings "+key)
		if inNow {
			db[key] = nv
		} else {
			delete(db, key)
		}
	}
	merged := maps.Clone(dbProjects)
	names := map[string]bool{}
	for name := range prevProjects {
		names[name] = true
	}
	for name := range fileProjects {
		names[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		pv, inPrev := prevProjects[name]
		nv, inNow := fileProjects[name]
		if inPrev == inNow && sameYAML(pv, string(nv)) {
			continue
		}
		changed = append(changed, "projects "+name)
		if inNow {
			merged[name] = string(nv)
		} else {
			delete(merged, name)
		}
	}
	if len(changed) == 0 {
		return dbCat, dbProjects, nil, nil
	}
	out, err := yaml.Marshal(db)
	return string(out), merged, changed, err
}

func unionKeys(a, b map[string]any) []string {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return slices.Sorted(maps.Keys(keys))
}

// sameJSON compares parsed YAML values (encoding/json sorts map keys).
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func sameYAML(a, b string) bool {
	var x, y any
	yaml.Unmarshal([]byte(a), &x)
	yaml.Unmarshal([]byte(b), &y)
	return sameJSON(x, y)
}

func docList(m map[string]string) [][]byte {
	var out [][]byte
	for _, name := range slices.Sorted(maps.Keys(m)) {
		out = append(out, []byte(m[name]))
	}
	return out
}

// FilesError is why changes in the config files were not applied at the
// last start, if they were not.
func (a *App) FilesError(ctx context.Context) string {
	v, _ := a.Store.GetKV(ctx, kvFilesError)
	return v
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

// RunsAffectedError refuses a config edit that would fail active runs with
// configuration drift; repeat it with force to apply it anyway.
type RunsAffectedError struct{ Runs []string }

func (e *RunsAffectedError) Error() string {
	return fmt.Sprintf("this change would fail %d active run(s) with configuration drift: %s", len(e.Runs), strings.Join(e.Runs, ", "))
}

// EditConfig applies edits together: they are validated as a whole against
// the environment, stored, and published to every component at once. Unless
// forced, it refuses (*RunsAffectedError) when the change would fail active
// runs. The warnings name saved flows that the change breaks.
func (a *App) EditConfig(ctx context.Context, force bool, edits ...ConfigEdit) (warnings []string, err error) {
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
	if !force && a.Engine != nil {
		drifted, err := a.Engine.DriftedRuns(ctx, next)
		if err != nil {
			return nil, err
		}
		if len(drifted) > 0 {
			return nil, &RunsAffectedError{Runs: drifted}
		}
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
