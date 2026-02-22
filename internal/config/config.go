package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Linear     LinearConfig     `yaml:"linear"`
	Pipeline   []StageConfig    `yaml:"pipeline"`
	Subprocess SubprocessConfig `yaml:"subprocess"`
	Workspace  WorkspaceConfig  `yaml:"workspace"`
}

type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type LinearConfig struct {
	APIKey             string        `yaml:"api_key"`
	WebhookSecret      string        `yaml:"webhook_secret"`
	TeamKey            string        `yaml:"team_key"`
	Mode               string        `yaml:"mode"`
	PollInterval       string        `yaml:"poll_interval"`
	ParsedPollInterval time.Duration `yaml:"-"`
}

type StageConfig struct {
	Name        string   `yaml:"name"`
	LinearState string   `yaml:"linear_state"`
	Command     string   `yaml:"command"`
	Args        []string `yaml:"args"`
	PromptFile  string   `yaml:"prompt_file"`
	Prompt      string   `yaml:"-"` // resolved from PromptFile at load time
	NextState   string   `yaml:"next_state"`
	Timeout     int      `yaml:"timeout"`
	Labels          []string `yaml:"labels"`
	CreatesPR       bool     `yaml:"creates_pr"`
	UsesBranch      bool     `yaml:"uses_branch"`
	FailureState    string   `yaml:"failure_state"`
	WaitForApproval bool     `yaml:"wait_for_approval"`
}

type SubprocessConfig struct {
	ContextMode   string `yaml:"context_mode"`
	MaxConcurrent int    `yaml:"max_concurrent"`
}

// Load reads and parses a YAML config file, expanding environment variables.
// Prompt file paths are resolved relative to the config file's directory.
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

	configDir := filepath.Dir(path)
	if err := cfg.validate(configDir); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate(configDir string) error {
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
	if c.Linear.TeamKey == "" {
		return fmt.Errorf("linear.team_key is required")
	}

	// Default mode to webhook
	if c.Linear.Mode == "" {
		c.Linear.Mode = "webhook"
	}
	switch c.Linear.Mode {
	case "webhook":
		if c.Linear.WebhookSecret == "" {
			return fmt.Errorf("linear.webhook_secret is required when mode is \"webhook\"")
		}
	case "poll":
		if c.Linear.PollInterval == "" {
			return fmt.Errorf("linear.poll_interval is required when mode is \"poll\"")
		}
		d, err := time.ParseDuration(c.Linear.PollInterval)
		if err != nil {
			return fmt.Errorf("linear.poll_interval: %w", err)
		}
		if d < 10*time.Second {
			return fmt.Errorf("linear.poll_interval must be at least 10s, got %s", d)
		}
		c.Linear.ParsedPollInterval = d

		// Warn about wait_for_approval in poll mode
		for _, stage := range c.Pipeline {
			if stage.WaitForApproval {
				slog.Warn("wait_for_approval has limited functionality in poll mode (comment re-runs won't auto-trigger)",
					"stage", stage.Name,
				)
			}
		}
	default:
		return fmt.Errorf("linear.mode must be \"webhook\" or \"poll\", got %q", c.Linear.Mode)
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

	// Create workspace root if configured
	if c.Workspace.Root != "" {
		if err := os.MkdirAll(c.Workspace.Root, 0755); err != nil {
			return fmt.Errorf("creating workspace root %q: %w", c.Workspace.Root, err)
		}
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
		if stage.PromptFile == "" {
			return fmt.Errorf("pipeline[%d].prompt_file is required", i)
		}
		promptPath := stage.PromptFile
		if !filepath.IsAbs(promptPath) {
			promptPath = filepath.Join(configDir, promptPath)
		}
		promptData, err := os.ReadFile(promptPath)
		if err != nil {
			return fmt.Errorf("pipeline[%d].prompt_file %q: %w", i, stage.PromptFile, err)
		}
		c.Pipeline[i].Prompt = string(promptData)

		if stage.NextState == "" {
			return fmt.Errorf("pipeline[%d].next_state is required", i)
		}
		if stage.Timeout == 0 {
			c.Pipeline[i].Timeout = 3600
		}
		if stage.UsesBranch && stage.CreatesPR {
			return fmt.Errorf("pipeline[%d] has both uses_branch and creates_pr (mutually exclusive)", i)
		}
		if stage.FailureState != "" && strings.EqualFold(stage.FailureState, stage.LinearState) {
			return fmt.Errorf("pipeline[%d] failure_state cannot equal linear_state", i)
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
