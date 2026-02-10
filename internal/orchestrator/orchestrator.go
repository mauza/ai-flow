package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/git"
	"github.com/mauza/ai-flow/internal/linear"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/subprocess"
)

// Orchestrator coordinates webhook events through the pipeline.
type Orchestrator struct {
	cfg    *config.Config
	client *linear.Client
	store  *store.Store
	runner *subprocess.Runner
	git    *git.Manager
}

// New creates a new Orchestrator.
func New(cfg *config.Config, client *linear.Client, store *store.Store, runner *subprocess.Runner, gitMgr *git.Manager) *Orchestrator {
	return &Orchestrator{
		cfg:    cfg,
		client: client,
		store:  store,
		runner: runner,
		git:    gitMgr,
	}
}

// HandleWebhook processes a validated webhook payload through the pipeline.
func (o *Orchestrator) HandleWebhook(ctx context.Context, payload linear.WebhookPayload) {
	// Parse issue data from payload
	var issue linear.IssueData
	if err := json.Unmarshal(payload.Data, &issue); err != nil {
		slog.Error("parsing issue data from webhook", "error", err)
		return
	}

	// Check if state actually changed
	var updatedFrom linear.UpdatedFromData
	if payload.UpdatedFrom != nil {
		if err := json.Unmarshal(payload.UpdatedFrom, &updatedFrom); err != nil {
			slog.Debug("parsing updatedFrom", "error", err)
		}
	}
	if updatedFrom.StateID == "" {
		slog.Debug("ignoring update without state change", "issue", issue.Identifier)
		return
	}

	// Resolve current state name from ID
	stateName, ok := o.client.ResolveStateName(issue.StateID)
	if !ok {
		slog.Warn("unknown state ID", "stateId", issue.StateID, "issue", issue.Identifier)
		return
	}

	slog.Info("issue state changed",
		"issue", issue.Identifier,
		"state", stateName,
	)

	// Find matching pipeline stage
	stage := o.cfg.FindStage(stateName)
	if stage == nil {
		slog.Debug("no pipeline stage for state", "state", stateName, "issue", issue.Identifier)
		return
	}

	// Fetch full issue details (needed for label name matching)
	details, err := o.client.GetIssue(ctx, issue.ID)
	if err != nil {
		slog.Error("fetching issue details", "error", err, "issue", issue.Identifier)
		return
	}

	// Collect label names
	var labelNames []string
	for _, l := range details.Labels.Nodes {
		labelNames = append(labelNames, l.Name)
	}

	// Check label filters using resolved label names
	if !matchesLabels(stage.Labels, labelNames) {
		slog.Debug("issue does not match label filter",
			"issue", issue.Identifier,
			"stage", stage.Name,
			"requiredLabels", stage.Labels,
			"issueLabels", labelNames,
		)
		return
	}

	// Dedup check
	runID, inserted, err := o.store.StartRun(issue.ID, stage.Name)
	if err != nil {
		slog.Error("dedup check failed", "error", err, "issue", issue.Identifier)
		return
	}
	if !inserted {
		slog.Info("run already in progress, skipping",
			"issue", issue.Identifier,
			"stage", stage.Name,
		)
		return
	}

	slog.Info("starting pipeline stage",
		"issue", issue.Identifier,
		"stage", stage.Name,
	)

	if stage.CreatesPR && o.git != nil {
		o.handleWithGit(ctx, runID, details, stage, stateName, labelNames)
	} else {
		o.handleWithoutGit(ctx, runID, details, stage, stateName, labelNames)
	}
}

func (o *Orchestrator) handleWithoutGit(ctx context.Context, runID int64, details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string) {
	input := o.buildInput(details, stage, stateName, labelNames)

	result, err := o.runner.Run(ctx, input)
	if err != nil {
		slog.Error("subprocess execution error",
			"error", err,
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.TimeoutRun(runID, err.Error())
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, err.Error())
		return
	}

	switch result.ExitCode {
	case 0:
		slog.Info("subprocess succeeded",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 0, result.Stdout, "")
		o.transitionAndComment(ctx, details.ID, details.Identifier, stage, result.Stdout, "")

	case 2:
		slog.Info("subprocess skipped",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 2, "skipped", "")

	default:
		slog.Warn("subprocess failed",
			"issue", details.Identifier,
			"stage", stage.Name,
			"exitCode", result.ExitCode,
			"stderr", result.Stderr,
		)
		errMsg := result.Stderr
		if errMsg == "" {
			errMsg = result.Stdout
		}
		o.store.FailRun(runID, result.ExitCode, errMsg)
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, errMsg)
	}
}

func (o *Orchestrator) handleWithGit(ctx context.Context, runID int64, details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string) {
	branchName := git.SanitizeBranchName(details.Identifier, details.Title)
	repo := o.cfg.Project.GithubRepo
	baseBranch := o.cfg.Project.DefaultBranch

	// Create temp directory for the clone
	tmpDir, err := os.MkdirTemp("", "aiflow-"+details.Identifier+"-*")
	if err != nil {
		slog.Error("creating temp dir", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "failed to create temp dir: "+err.Error())
		return
	}
	defer o.git.Cleanup(tmpDir)

	// Clone repository
	cloneCtx, cloneCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cloneCancel()
	if err := o.git.Clone(cloneCtx, repo, baseBranch, tmpDir); err != nil {
		slog.Error("cloning repo", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "failed to clone repo: "+err.Error())
		return
	}

	// Create feature branch
	if err := o.git.CreateBranch(ctx, tmpDir, branchName); err != nil {
		slog.Error("creating branch", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "failed to create branch: "+err.Error())
		return
	}

	// Run subprocess in the cloned repo
	input := o.buildInput(details, stage, stateName, labelNames)
	input.WorkDir = tmpDir
	input.BranchName = branchName

	result, err := o.runner.Run(ctx, input)
	if err != nil {
		slog.Error("subprocess execution error",
			"error", err,
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.TimeoutRun(runID, err.Error())
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, err.Error())
		return
	}

	switch result.ExitCode {
	case 0:
		prURL, err := o.commitAndCreatePR(ctx, tmpDir, branchName, baseBranch, details)
		if err != nil {
			slog.Error("creating PR", "error", err, "issue", details.Identifier)
			o.store.FailRun(runID, -1, err.Error())
			o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "subprocess succeeded but PR creation failed: "+err.Error())
			return
		}

		slog.Info("subprocess succeeded",
			"issue", details.Identifier,
			"stage", stage.Name,
			"prURL", prURL,
		)
		o.store.CompleteRun(runID, 0, result.Stdout, prURL)
		o.transitionAndComment(ctx, details.ID, details.Identifier, stage, result.Stdout, prURL)

	case 2:
		slog.Info("subprocess skipped",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 2, "skipped", "")

	default:
		slog.Warn("subprocess failed",
			"issue", details.Identifier,
			"stage", stage.Name,
			"exitCode", result.ExitCode,
			"stderr", result.Stderr,
		)
		errMsg := result.Stderr
		if errMsg == "" {
			errMsg = result.Stdout
		}
		o.store.FailRun(runID, result.ExitCode, errMsg)
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, errMsg)
	}
}

// commitAndCreatePR handles the git commit, push, and PR creation after a successful subprocess.
// Returns the PR URL, or empty string if there were no changes (still considered success).
func (o *Orchestrator) commitAndCreatePR(ctx context.Context, dir, branch, baseBranch string, details *linear.IssueDetails) (string, error) {
	hasChanges, err := o.git.HasChanges(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("checking for changes: %w", err)
	}
	if !hasChanges {
		slog.Info("no changes after subprocess", "issue", details.Identifier)
		return "", nil
	}

	commitMsg := fmt.Sprintf("%s: %s\n\nGenerated by ai-flow", details.Identifier, details.Title)
	if err := o.git.CommitAll(ctx, dir, commitMsg); err != nil {
		return "", fmt.Errorf("committing changes: %w", err)
	}

	pushCtx, pushCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer pushCancel()
	if err := o.git.Push(pushCtx, dir, branch); err != nil {
		return "", fmt.Errorf("pushing branch: %w", err)
	}

	prTitle := fmt.Sprintf("%s: %s", details.Identifier, details.Title)
	prBody := fmt.Sprintf("Generated by ai-flow\n\nLinear issue: %s", details.URL)
	prURL, err := o.git.CreatePR(ctx, dir, prTitle, prBody, baseBranch, branch)
	if err != nil {
		return "", fmt.Errorf("creating PR: %w", err)
	}

	return prURL, nil
}

func (o *Orchestrator) buildInput(details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string) subprocess.Input {
	return subprocess.Input{
		IssueID:          details.ID,
		IssueIdentifier:  details.Identifier,
		IssueTitle:       details.Title,
		IssueDescription: details.Description,
		IssueURL:         details.URL,
		IssueState:       stateName,
		IssueLabels:      labelNames,
		StageName:        stage.Name,
		NextState:        stage.NextState,
		Prompt:           stage.Prompt,
		Command:          stage.Command,
		Args:             stage.Args,
		Timeout:          time.Duration(stage.Timeout) * time.Second,
		ContextMode:      o.cfg.Subprocess.ContextMode,
	}
}

func matchesLabels(required, issueLabels []string) bool {
	if len(required) == 0 {
		return true
	}
	labelSet := make(map[string]bool, len(issueLabels))
	for _, l := range issueLabels {
		labelSet[strings.ToLower(l)] = true
	}
	for _, req := range required {
		if labelSet[strings.ToLower(req)] {
			return true
		}
	}
	return false
}

func (o *Orchestrator) transitionAndComment(ctx context.Context, issueID, identifier string, stage *config.StageConfig, output, prURL string) {
	nextStateID, ok := o.client.ResolveStateID(stage.NextState)
	if !ok {
		slog.Error("cannot resolve next state",
			"nextState", stage.NextState,
			"issue", identifier,
		)
		return
	}

	if err := o.client.UpdateIssueState(ctx, issueID, nextStateID); err != nil {
		slog.Error("transitioning issue",
			"error", err,
			"issue", identifier,
			"nextState", stage.NextState,
		)
		return
	}

	slog.Info("transitioned issue",
		"issue", identifier,
		"to", stage.NextState,
	)

	// Post output as comment (truncate if very long)
	comment := formatSuccessComment(stage.Name, output, prURL)
	if err := o.client.PostComment(ctx, issueID, comment); err != nil {
		slog.Error("posting comment", "error", err, "issue", identifier)
	}
}

func (o *Orchestrator) postFailureComment(ctx context.Context, issueID, identifier, stageName, errMsg string) {
	comment := fmt.Sprintf("**ai-flow: stage `%s` failed**\n\n```\n%s\n```", stageName, truncate(errMsg, 3000))
	if err := o.client.PostComment(ctx, issueID, comment); err != nil {
		slog.Error("posting failure comment", "error", err, "issue", identifier)
	}
}

func formatSuccessComment(stageName, output, prURL string) string {
	output = strings.TrimSpace(output)

	var parts []string
	if prURL != "" {
		parts = append(parts, fmt.Sprintf("**ai-flow: stage `%s` completed**\n\n**PR:** %s", stageName, prURL))
	} else if output == "" {
		return fmt.Sprintf("**ai-flow: stage `%s` completed** (no output)", stageName)
	} else {
		parts = append(parts, fmt.Sprintf("**ai-flow: stage `%s` completed**", stageName))
	}

	if output != "" {
		parts = append(parts, truncate(output, 10000))
	}

	return strings.Join(parts, "\n\n")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}
