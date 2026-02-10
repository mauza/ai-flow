package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Linear     LinearConfig     `yaml:"linear"`
	Project    *ProjectConfig   `yaml:"project"`
	Pipeline   []StageConfig    `yaml:"pipeline"`
	Subprocess SubprocessConfig `yaml:"subprocess"`
}

type ProjectConfig struct {
	GithubRepo    string `yaml:"github_repo"`
	DefaultBranch string `yaml:"default_branch"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type LinearConfig struct {
	APIKey        string `yaml:"api_key"`
	WebhookSecret string `yaml:"webhook_secret"`
	TeamKey       string `yaml:"team_key"`
}

type StageConfig struct {
	Name        string   `yaml:"name"`
	LinearState string   `yaml:"linear_state"`
	Command     string   `yaml:"command"`
	Args        []string `yaml:"args"`
	Prompt      string   `yaml:"prompt"`
	NextState   string   `yaml:"next_state"`
	Timeout     int      `yaml:"timeout"`
	Labels      []string `yaml:"labels"`
	CreatesPR   bool     `yaml:"creates_pr"`
}

type SubprocessConfig struct {
	ContextMode   string `yaml:"context_mode"`
	MaxConcurrent int    `yaml:"max_concurrent"`
}

// Load reads and parses a YAML config file, expanding environment variables.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	// Defaults
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Subprocess.ContextMode == "" {
		c.Subprocess.ContextMode = "env"
	}
	if c.Subprocess.MaxConcurrent == 0 {
		c.Subprocess.MaxConcurrent = 3
	}

	// Required fields
	if c.Linear.APIKey == "" {
		return fmt.Errorf("linear.api_key is required")
	}
	if c.Linear.WebhookSecret == "" {
		return fmt.Errorf("linear.webhook_secret is required")
	}
	if c.Linear.TeamKey == "" {
		return fmt.Errorf("linear.team_key is required")
	}

	if len(c.Pipeline) == 0 {
		return fmt.Errorf("at least one pipeline stage is required")
	}

	// Validate context_mode
	switch c.Subprocess.ContextMode {
	case "env", "stdin", "both":
	default:
		return fmt.Errorf("subprocess.context_mode must be env, stdin, or both; got %q", c.Subprocess.ContextMode)
	}

	// Default project settings
	if c.Project != nil && c.Project.DefaultBranch == "" {
		c.Project.DefaultBranch = "main"
	}

	// Check stages and no duplicate linear_states
	seen := make(map[string]bool)
	for i, stage := range c.Pipeline {
		if stage.Name == "" {
			return fmt.Errorf("pipeline[%d].name is required", i)
		}
		if stage.LinearState == "" {
			return fmt.Errorf("pipeline[%d].linear_state is required", i)
		}
		if stage.Command == "" {
			return fmt.Errorf("pipeline[%d].command is required", i)
		}
		if stage.NextState == "" {
			return fmt.Errorf("pipeline[%d].next_state is required", i)
		}
		if stage.Timeout == 0 {
			c.Pipeline[i].Timeout = 300
		}
		if stage.CreatesPR {
			if c.Project == nil || c.Project.GithubRepo == "" {
				return fmt.Errorf("pipeline[%d] has creates_pr but project.github_repo is not configured", i)
			}
		}
		if seen[stage.LinearState] {
			return fmt.Errorf("duplicate linear_state %q in pipeline", stage.LinearState)
		}
		seen[stage.LinearState] = true
	}

	return nil
}

// FindStage returns the pipeline stage matching the given Linear state name, or nil.
func (c *Config) FindStage(linearStateName string) *StageConfig {
	for i := range c.Pipeline {
		if strings.EqualFold(c.Pipeline[i].LinearState, linearStateName) {
			return &c.Pipeline[i]
		}
	}
	return nil
}
