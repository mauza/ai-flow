package linear

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const branchMetadataMarker = "<!-- ai-flow-branch-metadata -->"

var branchMetadataBlock = regexp.MustCompile(`(?s)\n*` + regexp.QuoteMeta(branchMetadataMarker) + `.*$`)

// AppendBranchMetadata appends (or replaces) a branch metadata block at the end
// of an issue description. The block is idempotent — calling it again with different
// values replaces the previous block.
func AppendBranchMetadata(description, branchName, prURL string) string {
	// Remove existing metadata block if present
	description = branchMetadataBlock.ReplaceAllString(description, "")

	var block strings.Builder
	block.WriteString("\n\n")
	block.WriteString(branchMetadataMarker)
	block.WriteString("\n---\n")
	block.WriteString(fmt.Sprintf("**Branch:** `%s`", branchName))
	if prURL != "" {
		block.WriteString(fmt.Sprintf("\n**PR:** %s", prURL))
	}

	return description + block.String()
}

// ProjectMeta holds GitHub repository metadata parsed from a Linear project description.
type ProjectMeta struct {
	GithubRepo    string `yaml:"github_repo" json:"github_repo"`
	DefaultBranch string `yaml:"default_branch" json:"default_branch"`
}

// ParseProjectMeta extracts project metadata from a Linear project description.
// It tries JSON first (since Linear's editor mangles YAML frontmatter), then
// falls back to YAML frontmatter delimited by "---" lines.
// If default_branch is not set, it defaults to "main".
func ParseProjectMeta(description string) (*ProjectMeta, error) {
	description = strings.TrimSpace(description)

	// Try JSON first
	if meta, err := parseProjectMetaJSON(description); err == nil {
		return meta, nil
	}

	// Fall back to YAML frontmatter
	return parseProjectMetaYAML(description)
}

func parseProjectMetaJSON(description string) (*ProjectMeta, error) {
	var meta ProjectMeta
	if err := json.Unmarshal([]byte(description), &meta); err != nil {
		return nil, err
	}
	if meta.GithubRepo == "" {
		return nil, fmt.Errorf("github_repo is required in project metadata")
	}
	if meta.DefaultBranch == "" {
		meta.DefaultBranch = "main"
	}
	return &meta, nil
}

func parseProjectMetaYAML(description string) (*ProjectMeta, error) {
	const delimiter = "---"

	lines := strings.Split(description, "\n")

	// Find opening delimiter
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == delimiter {
			start = i
			break
		}
	}
	if start == -1 {
		return nil, fmt.Errorf("no project metadata found in description (expected JSON or YAML frontmatter)")
	}

	// Find closing delimiter
	end := -1
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == delimiter {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, fmt.Errorf("no closing --- delimiter in project description frontmatter")
	}

	frontmatter := strings.Join(lines[start+1:end], "\n")

	var meta ProjectMeta
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return nil, fmt.Errorf("parsing project frontmatter: %w", err)
	}

	if meta.GithubRepo == "" {
		return nil, fmt.Errorf("github_repo is required in project frontmatter")
	}

	if meta.DefaultBranch == "" {
		meta.DefaultBranch = "main"
	}

	return &meta, nil
}
