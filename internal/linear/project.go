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
	block.WriteString("\n")
	block.WriteString(fmt.Sprintf("**Branch:** `%s`", branchName))
	if prURL != "" {
		block.WriteString(fmt.Sprintf("\n**PR:** %s", prURL))
	}

	return description + block.String()
}

// IssueMeta holds GitHub repository metadata parsed from a Linear issue description.
type IssueMeta struct {
	GithubRepo    string `yaml:"github_repo" json:"github_repo"`
	DefaultBranch string `yaml:"default_branch" json:"default_branch"`
}

// ParseIssueMeta extracts repository metadata from a Linear issue description.
// It looks for a YAML frontmatter block delimited by "---" lines, or a JSON object
// embedded in the description. If default_branch is not set, it defaults to "main".
func ParseIssueMeta(description string) (*IssueMeta, error) {
	description = strings.TrimSpace(description)

	// Try YAML frontmatter first (most natural for issue descriptions)
	if meta, err := parseIssueMetaYAML(description); err == nil {
		return meta, nil
	}

	// Fall back to JSON
	return parseIssueMetaJSON(description)
}

func parseIssueMetaJSON(description string) (*IssueMeta, error) {
	// Extract just the first JSON object from the description,
	// ignoring any trailing content (e.g. branch metadata, markdown).
	start := strings.Index(description, "{")
	if start == -1 {
		return nil, fmt.Errorf("no JSON object found in description")
	}
	end := strings.Index(description[start:], "}")
	if end == -1 {
		return nil, fmt.Errorf("no closing brace found in description")
	}
	jsonStr := description[start : start+end+1]

	var meta IssueMeta
	if err := json.Unmarshal([]byte(jsonStr), &meta); err != nil {
		return nil, err
	}
	if meta.GithubRepo == "" {
		return nil, fmt.Errorf("github_repo is required in issue metadata")
	}
	if meta.DefaultBranch == "" {
		meta.DefaultBranch = "main"
	}
	return &meta, nil
}

func parseIssueMetaYAML(description string) (*IssueMeta, error) {
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
		return nil, fmt.Errorf("no metadata found in issue description (expected YAML frontmatter or JSON)")
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
		return nil, fmt.Errorf("no closing --- delimiter in issue description frontmatter")
	}

	frontmatter := strings.Join(lines[start+1:end], "\n")

	var meta IssueMeta
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return nil, fmt.Errorf("parsing issue frontmatter: %w", err)
	}

	if meta.GithubRepo == "" {
		return nil, fmt.Errorf("github_repo is required in issue frontmatter")
	}

	if meta.DefaultBranch == "" {
		meta.DefaultBranch = "main"
	}

	return &meta, nil
}
