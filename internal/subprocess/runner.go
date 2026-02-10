package subprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Input contains everything needed to run a subprocess for a pipeline stage.
type Input struct {
	// Issue context
	IssueID          string
	IssueIdentifier  string
	IssueTitle       string
	IssueDescription string
	IssueURL         string
	IssueState       string
	IssueLabels      []string

	// Stage config
	StageName   string
	NextState   string
	Prompt      string
	Command     string
	Args        []string
	Timeout     time.Duration
	ContextMode string // "env", "stdin", "both"

	// Git context (set when stage creates a PR)
	WorkDir    string
	BranchName string
}

// Result captures the outcome of a subprocess run.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Runner manages subprocess execution with concurrency control.
type Runner struct {
	sem chan struct{}
}

// NewRunner creates a runner with the given max concurrency.
func NewRunner(maxConcurrent int) *Runner {
	return &Runner{
		sem: make(chan struct{}, maxConcurrent),
	}
}

// Run executes a subprocess with the given input, respecting concurrency limits.
func (r *Runner) Run(ctx context.Context, input Input) (*Result, error) {
	// Acquire semaphore
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Build timeout context
	ctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()

	// Compose the full prompt with issue context
	composedPrompt := composePrompt(input)

	// Build command args: configured args + composed prompt as final arg
	args := make([]string, len(input.Args))
	copy(args, input.Args)
	args = append(args, composedPrompt)

	cmd := exec.CommandContext(ctx, input.Command, args...)

	// Set working directory for git-managed runs
	if input.WorkDir != "" {
		cmd.Dir = input.WorkDir
	}

	// Set environment variables
	cmd.Env = buildEnv(input, composedPrompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Optionally pipe JSON to stdin
	if input.ContextMode == "stdin" || input.ContextMode == "both" {
		stdinData, err := json.Marshal(map[string]any{
			"issue_id":          input.IssueID,
			"issue_identifier":  input.IssueIdentifier,
			"issue_title":       input.IssueTitle,
			"issue_description": input.IssueDescription,
			"issue_url":         input.IssueURL,
			"issue_state":       input.IssueState,
			"issue_labels":      input.IssueLabels,
			"stage_name":        input.StageName,
			"next_state":        input.NextState,
			"prompt":            input.Prompt,
		})
		if err != nil {
			return nil, fmt.Errorf("marshaling stdin: %w", err)
		}
		cmd.Stdin = bytes.NewReader(stdinData)
	}

	err := cmd.Run()

	result := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			return result, fmt.Errorf("subprocess timed out after %s", input.Timeout)
		} else {
			return result, fmt.Errorf("executing subprocess: %w", err)
		}
	}

	return result, nil
}

func composePrompt(input Input) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Issue: %s - %s\n", input.IssueIdentifier, input.IssueTitle))
	if input.IssueDescription != "" {
		b.WriteString(fmt.Sprintf("Description: %s\n", input.IssueDescription))
	}
	if input.IssueURL != "" {
		b.WriteString(fmt.Sprintf("URL: %s\n", input.IssueURL))
	}
	if len(input.IssueLabels) > 0 {
		b.WriteString(fmt.Sprintf("Labels: %s\n", strings.Join(input.IssueLabels, ", ")))
	}
	b.WriteString("\n---\n\n")
	b.WriteString(input.Prompt)
	return b.String()
}

func buildEnv(input Input, composedPrompt string) []string {
	// Inherit the parent process environment
	env := os.Environ()

	// Append AIFLOW-specific variables
	env = append(env,
		"AIFLOW_ISSUE_ID="+input.IssueID,
		"AIFLOW_ISSUE_IDENTIFIER="+input.IssueIdentifier,
		"AIFLOW_ISSUE_TITLE="+input.IssueTitle,
		"AIFLOW_ISSUE_DESCRIPTION="+input.IssueDescription,
		"AIFLOW_ISSUE_URL="+input.IssueURL,
		"AIFLOW_ISSUE_STATE="+input.IssueState,
		"AIFLOW_ISSUE_LABELS="+strings.Join(input.IssueLabels, ","),
		"AIFLOW_STAGE_NAME="+input.StageName,
		"AIFLOW_NEXT_STATE="+input.NextState,
		"AIFLOW_PROMPT="+composedPrompt,
	)
	if input.WorkDir != "" {
		env = append(env, "AIFLOW_WORK_DIR="+input.WorkDir)
	}
	if input.BranchName != "" {
		env = append(env, "AIFLOW_BRANCH="+input.BranchName)
	}
	return env
}
