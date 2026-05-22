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
	meta, err := ParseIssueMetaRelaxed(description)
	if err != nil {
		return nil, err
	}
	return NormalizeIssueMeta(meta, "")
}

// ParseIssueMetaRelaxed parses structured issue metadata when present.
// It defaults default_branch to main and allows github_repo to be empty.
func ParseIssueMetaRelaxed(description string) (*IssueMeta, error) {
	description = strings.TrimSpace(description)

	// Try YAML frontmatter first (most natural for issue descriptions)
	meta, err := parseIssueMetaYAML(description)
	if err == nil {
		return meta, nil
	}
	if strings.HasPrefix(description, "---") {
		return nil, err
	}

	// Fall back to JSON
	meta, err = parseIssueMetaJSON(description)
	if err == nil {
		return meta, nil
	}
	if strings.Contains(description, "{") {
		return nil, err
	}

	return &IssueMeta{DefaultBranch: "main"}, nil
}

// NormalizeIssueMeta fills defaults and applies an optional fallback owner.
// When GithubRepo is just a repo name, fallbackOwner expands it to owner/repo.
func NormalizeIssueMeta(meta *IssueMeta, fallbackOwner string) (*IssueMeta, error) {
	if meta == nil {
		return nil, fmt.Errorf("issue metadata is required")
	}

	meta.GithubRepo = strings.TrimSpace(meta.GithubRepo)
	meta.DefaultBranch = strings.TrimSpace(meta.DefaultBranch)
	fallbackOwner = strings.TrimSpace(fallbackOwner)

	if meta.GithubRepo == "" {
		return nil, fmt.Errorf("github_repo is required")
	}

	if !strings.Contains(meta.GithubRepo, "/") {
		if fallbackOwner == "" {
			return nil, fmt.Errorf("github_repo %q must be owner/repo or config github.owner must be set", meta.GithubRepo)
		}
		meta.GithubRepo = fallbackOwner + "/" + meta.GithubRepo
	}

	if meta.DefaultBranch == "" {
		meta.DefaultBranch = "main"
	}

	return meta, nil
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
	if strings.TrimSpace(meta.DefaultBranch) == "" {
		meta.DefaultBranch = "main"
	}
	return &meta, nil
}

func parseIssueMetaYAML(description string) (*IssueMeta, error) {
	const delimiter = "---"

	lines := strings.Split(description, "\n")

	// Find opening delimiter — either "---" or "```yaml" / "```" (Linear renders
	// raw YAML frontmatter as a fenced code block when storing the description).
	start := -1
	closingDelimiter := delimiter
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == delimiter {
			start = i
			closingDelimiter = delimiter
			break
		}
		if trimmed == "```yaml" || trimmed == "```yml" || trimmed == "```" {
			start = i
			closingDelimiter = "```"
			break
		}
	}
	if start == -1 {
		return nil, fmt.Errorf("no metadata found in issue description (expected YAML frontmatter or JSON)")
	}

	// Find closing delimiter
	end := -1
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == closingDelimiter {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, fmt.Errorf("no closing %s delimiter in issue description frontmatter", closingDelimiter)
	}

	frontmatter := strings.Join(lines[start+1:end], "\n")

	var meta IssueMeta
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return nil, fmt.Errorf("parsing issue frontmatter: %w", err)
	}

	if strings.TrimSpace(meta.DefaultBranch) == "" {
		meta.DefaultBranch = "main"
	}
	return &meta, nil
}
