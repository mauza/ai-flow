package subprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxOutputBytes = 1 << 20 // 1 MB per stream

// limitedWriter wraps a bytes.Buffer and stops writing after a limit.
type limitedWriter struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.dropped += len(p)
		return len(p), nil // discard silently to avoid blocking subprocess
	}
	if len(p) > remaining {
		w.dropped += len(p) - remaining
		p = p[:remaining]
	}
	return w.buf.Write(p)
}

func (w *limitedWriter) String() string {
	s := w.buf.String()
	if w.dropped > 0 {
		return s + fmt.Sprintf("\n... (%d bytes truncated)", w.dropped)
	}
	return s
}

var _ io.Writer = (*limitedWriter)(nil)

// Comment represents a human comment on an issue.
type Comment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

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

	// Comments from the issue (filtered, human-only)
	Comments []Comment
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

	stdout := &limitedWriter{limit: maxOutputBytes}
	stderr := &limitedWriter{limit: maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Optionally pipe JSON to stdin
	if input.ContextMode == "stdin" || input.ContextMode == "both" {
		stdinMap := map[string]any{
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
		}
		if len(input.Comments) > 0 {
			stdinMap["comments"] = input.Comments
		}
		stdinData, err := json.Marshal(stdinMap)
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

	if len(input.Comments) > 0 {
		b.WriteString("\n\n---\n\nComments:\n")
		for _, c := range input.Comments {
			b.WriteString(fmt.Sprintf("\n[%s]:\n%s\n", c.Author, c.Body))
		}
	}

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
	if len(input.Comments) > 0 {
		if commentsJSON, err := json.Marshal(input.Comments); err == nil {
			env = append(env, "AIFLOW_COMMENTS="+string(commentsJSON))
		}
	}
	return env
}
