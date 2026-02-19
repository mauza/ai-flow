package linear

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProjectMeta holds GitHub repository metadata parsed from a Linear project description.
type ProjectMeta struct {
	GithubRepo    string `yaml:"github_repo"`
	DefaultBranch string `yaml:"default_branch"`
}

// ParseProjectMeta extracts YAML frontmatter from a Linear project description.
// The frontmatter must be delimited by "---" lines. If default_branch is not set,
// it defaults to "main".
func ParseProjectMeta(description string) (*ProjectMeta, error) {
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
		return nil, fmt.Errorf("no YAML frontmatter found in project description")
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
