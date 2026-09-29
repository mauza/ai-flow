package planner

import (
	"slices"

	"github.com/mauza/ai-flow/internal/config"
)

// Eligibility is the project-filtered catalog menu shared by the planner and UI.
// Grant entries are usable YAML expressions, including explicit git modes.
type Eligibility struct {
	AllowedModels []string `json:"allowed_models"`
	AllowedGrants []string `json:"allowed_grants"`
}

// ProjectEligibility uses the same project predicates and git-mode checks as
// resolve.Validate. Bare git grants mean read, but are checked against the
// project policy as written, exactly as the resolver does.
func ProjectEligibility(cfg *config.Config, project *config.Project) Eligibility {
	out := Eligibility{AllowedModels: []string{}, AllowedGrants: []string{}}
	for _, name := range sortedKeys(cfg.Catalog.Models) {
		if project.AllowsModel(name) {
			out.AllowedModels = append(out.AllowedModels, name)
		}
	}
	for _, name := range sortedKeys(cfg.Catalog.Grants) {
		g := cfg.Catalog.Grants[name]
		if g.Kind != config.GrantGit {
			if project.AllowsGrant(name) {
				out.AllowedGrants = append(out.AllowedGrants, name)
			}
			continue
		}
		for _, mode := range []string{"", "read", "write"} {
			entry, effectiveMode := name, mode
			if mode == "" {
				effectiveMode = "read"
			} else {
				entry += ":" + mode
			}
			if project.AllowsGrant(entry) && (len(g.Modes) == 0 || slices.Contains(g.Modes, effectiveMode)) {
				out.AllowedGrants = append(out.AllowedGrants, entry)
			}
		}
	}
	return out
}
