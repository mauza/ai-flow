package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// workspacePath returns the persistent workspace directory for a repo+branch,
// or empty string if workspace root is not configured (fallback to temp dirs).
func (o *Orchestrator) workspacePath(repo, branch string) string {
	if o.cfg.Workspace.Root == "" {
		return ""
	}
	return filepath.Join(o.cfg.Workspace.Root, repo, branch)
}

// setupWorkspace prepares a workspace directory for a git operation.
// If persistent workspaces are configured, it reuses or creates the workspace.
// Otherwise, it creates a temp directory. Returns the work directory and a cleanup
// function (no-op for persistent workspaces).
func (o *Orchestrator) setupWorkspace(ctx context.Context, repo, baseBranch, targetBranch, identifier string) (workDir string, cleanup func(), err error) {
	wsPath := o.workspacePath(repo, targetBranch)
	if wsPath != "" {
		if err := os.MkdirAll(filepath.Dir(wsPath), 0755); err != nil {
			return "", nil, fmt.Errorf("creating workspace parent: %w", err)
		}

		gitDir := filepath.Join(wsPath, ".git")
		if info, statErr := os.Stat(gitDir); statErr == nil && info.IsDir() {
			// Existing workspace: fetch + reset to clean state
			slog.Info("reusing persistent workspace", "path", wsPath, "issue", identifier)
			if err := o.git.Fetch(ctx, wsPath); err != nil {
				return "", nil, fmt.Errorf("fetching in workspace: %w", err)
			}
			if err := o.git.ResetToRemote(ctx, wsPath, targetBranch); err != nil {
				return "", nil, fmt.Errorf("resetting workspace: %w", err)
			}
			return wsPath, func() {}, nil
		}

		// First time: clone into workspace dir
		cloneCtx, cloneCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cloneCancel()
		if err := o.git.Clone(cloneCtx, repo, baseBranch, wsPath); err != nil {
			return "", nil, fmt.Errorf("cloning into workspace: %w", err)
		}
		return wsPath, func() {}, nil
	}

	// Fallback: temp dir
	tmpDir, err := os.MkdirTemp("", "aiflow-"+identifier+"-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}
	cloneCtx, cloneCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cloneCancel()
	if err := o.git.Clone(cloneCtx, repo, baseBranch, tmpDir); err != nil {
		o.git.Cleanup(tmpDir)
		return "", nil, fmt.Errorf("cloning repo: %w", err)
	}
	return tmpDir, func() { o.git.Cleanup(tmpDir) }, nil
}

// cleanupWorkspaceIfDone removes the persistent workspace directory when the
// issue transitions to the Done state.
func (o *Orchestrator) cleanupWorkspaceIfDone(stage *config.StageConfig, repo, branchName string) {
	if !strings.EqualFold(stage.NextState, "Done") {
		return
	}
	wsPath := o.workspacePath(repo, branchName)
	if wsPath == "" {
		return
	}
	slog.Info("cleaning up workspace (issue done)", "path", wsPath)
	os.RemoveAll(wsPath)
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

	if stage.UsesBranch && o.git != nil {
		o.handleWithExistingBranch(ctx, runID, details, stage, stateName, labelNames)
	} else if stage.CreatesPR && o.git != nil {
		o.handleWithGit(ctx, runID, details, stage, stateName, labelNames)
	} else {
		o.handleWithoutGit(ctx, runID, details, stage, stateName, labelNames)
	}
}

func (o *Orchestrator) handleWithoutGit(ctx context.Context, runID int64, details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string) {
	input := o.buildInput(details, stage, stateName, labelNames)

	// Fetch cross-stage comments for context
	commentNodes, err := o.client.GetIssueComments(ctx, details.ID)
	if err != nil {
		slog.Warn("fetching cross-stage comments", "error", err, "issue", details.Identifier)
	} else if len(commentNodes) > 0 {
		input.Comments = convertComments(commentNodes)
	}

	result, err := o.runner.Run(ctx, input)
	if err != nil {
		slog.Error("subprocess execution error",
			"error", err,
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.TimeoutRun(runID, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, err.Error())
		return
	}

	switch result.ExitCode {
	case 0:
		slog.Info("subprocess succeeded",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 0, result.Stdout, "", "")
		if stage.WaitForApproval {
			comment := formatSuccessComment(stage.Name, result.Stdout, "")
			if err := o.client.PostComment(ctx, details.ID, comment); err != nil {
				slog.Error("posting comment", "error", err, "issue", details.Identifier)
			}
		} else {
			o.transitionAndComment(ctx, details.ID, details.Identifier, stage, result.Stdout, "")
		}

	case 2:
		slog.Info("subprocess skipped",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 2, "skipped", "", "")

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
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, errMsg)
	}
}

// resolveRepoConfig extracts GitHub repo metadata from the issue's Linear project.
func resolveRepoConfig(details *linear.IssueDetails) (repo, branch string, err error) {
	if details.Project == nil {
		return "", "", fmt.Errorf("issue %s has no Linear project", details.Identifier)
	}
	meta, err := linear.ParseProjectMeta(details.Project.Description)
	if err != nil {
		return "", "", fmt.Errorf("issue %s: project %q: %w", details.Identifier, details.Project.Name, err)
	}
	return meta.GithubRepo, meta.DefaultBranch, nil
}

func (o *Orchestrator) handleWithGit(ctx context.Context, runID int64, details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string) {
	branchName := git.SanitizeBranchName(details.Identifier, details.Title)
	repo, baseBranch, err := resolveRepoConfig(details)
	if err != nil {
		slog.Error("resolving repo config", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, err.Error())
		return
	}

	// Set up workspace (persistent or temp)
	workDir, cleanup, err := o.setupWorkspace(ctx, repo, baseBranch, branchName, details.Identifier)
	if err != nil {
		slog.Error("setting up workspace", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, "failed to set up workspace: "+err.Error())
		return
	}
	defer cleanup()

	// Check if branch already exists on remote (cycling case: security failed → back to implement)
	branchExists, err := o.git.BranchExistsOnRemote(ctx, workDir, branchName)
	if err != nil {
		slog.Warn("checking remote branch", "error", err, "issue", details.Identifier)
		branchExists = false
	}

	// Look up existing PR URL from previous runs
	prURL := ""
	if branchExists {
		if prevRun, err := o.store.GetFirstBranchForIssue(details.ID); err == nil && prevRun != nil {
			prURL = prevRun.PRURL
		}
		// If workspace already has the branch checked out (persistent), skip fetch+checkout
		if o.workspacePath(repo, branchName) == "" {
			if err := o.git.FetchAndCheckout(ctx, workDir, branchName); err != nil {
				slog.Error("fetching existing branch", "error", err, "issue", details.Identifier, "branch", branchName)
				o.store.FailRun(runID, -1, err.Error())
				o.failAndTransition(ctx, details.ID, details.Identifier, stage, "failed to fetch existing branch: "+err.Error())
				return
			}
		}
		slog.Info("reusing existing branch", "branch", branchName, "issue", details.Identifier)
	} else {
		if err := o.git.CreateBranch(ctx, workDir, branchName); err != nil {
			slog.Error("creating branch", "error", err, "issue", details.Identifier)
			o.store.FailRun(runID, -1, err.Error())
			o.failAndTransition(ctx, details.ID, details.Identifier, stage, "failed to create branch: "+err.Error())
			return
		}
	}

	// Run subprocess in the workspace
	input := o.buildInput(details, stage, stateName, labelNames)
	input.WorkDir = workDir
	input.BranchName = branchName

	// Fetch cross-stage comments for context
	commentNodes, err := o.client.GetIssueComments(ctx, details.ID)
	if err != nil {
		slog.Warn("fetching cross-stage comments", "error", err, "issue", details.Identifier)
	} else if len(commentNodes) > 0 {
		input.Comments = convertComments(commentNodes)
	}

	result, err := o.runner.Run(ctx, input)
	if err != nil {
		slog.Error("subprocess execution error",
			"error", err,
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.TimeoutRun(runID, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, err.Error())
		return
	}

	switch result.ExitCode {
	case 0:
		if branchExists {
			// Push to existing branch (PR auto-updates)
			pushed, err := o.commitAndPush(ctx, workDir, branchName, details, stage.Name)
			if err != nil {
				slog.Error("commit and push failed (cycling)", "error", err, "issue", details.Identifier)
				o.store.FailRun(runID, -1, err.Error())
				o.failAndTransition(ctx, details.ID, details.Identifier, stage, "subprocess succeeded but push failed: "+err.Error())
				return
			}
			if pushed && prURL != "" {
				o.commentOnPR(ctx, workDir, prURL, stage.Name, details.Identifier)
			}
		} else {
			var err error
			prURL, err = o.commitAndCreatePR(ctx, workDir, branchName, baseBranch, details)
			if err != nil {
				slog.Error("creating PR", "error", err, "issue", details.Identifier)
				o.store.FailRun(runID, -1, err.Error())
				o.failAndTransition(ctx, details.ID, details.Identifier, stage, "subprocess succeeded but PR creation failed: "+err.Error())
				return
			}

			// Write branch metadata to issue description
			if prURL != "" {
				newDesc := linear.AppendBranchMetadata(details.Description, branchName, prURL)
				if err := o.client.UpdateIssueDescription(ctx, details.ID, newDesc); err != nil {
					slog.Warn("updating issue description with branch metadata", "error", err, "issue", details.Identifier)
				}
			}
		}

		slog.Info("subprocess succeeded",
			"issue", details.Identifier,
			"stage", stage.Name,
			"prURL", prURL,
		)
		o.store.CompleteRun(runID, 0, result.Stdout, prURL, branchName)
		if stage.WaitForApproval {
			comment := formatSuccessComment(stage.Name, result.Stdout, prURL)
			if err := o.client.PostComment(ctx, details.ID, comment); err != nil {
				slog.Error("posting comment", "error", err, "issue", details.Identifier)
			}
		} else {
			o.transitionAndComment(ctx, details.ID, details.Identifier, stage, result.Stdout, prURL)
			o.cleanupWorkspaceIfDone(stage, repo, branchName)
		}

	case 2:
		slog.Info("subprocess skipped",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 2, "skipped", "", branchName)

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
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, errMsg)
	}
}

func (o *Orchestrator) handleWithExistingBranch(ctx context.Context, runID int64, details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string) {
	repo, baseBranch, err := resolveRepoConfig(details)
	if err != nil {
		slog.Error("resolving repo config", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, err.Error())
		return
	}

	// Look up branch from any previous run for this issue
	prevRun, err := o.store.GetFirstBranchForIssue(details.ID)
	if err != nil {
		slog.Error("looking up branch for issue", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, "failed to look up branch: "+err.Error())
		return
	}
	if prevRun == nil || prevRun.BranchName == "" {
		errMsg := "no existing branch found for this issue"
		slog.Error(errMsg, "issue", details.Identifier, "stage", stage.Name)
		o.store.FailRun(runID, -1, errMsg)
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, errMsg)
		return
	}

	branchName := prevRun.BranchName
	prURL := prevRun.PRURL

	// Set up workspace (persistent or temp)
	workDir, cleanup, err := o.setupWorkspace(ctx, repo, baseBranch, branchName, details.Identifier)
	if err != nil {
		slog.Error("setting up workspace", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, "failed to set up workspace: "+err.Error())
		return
	}
	defer cleanup()

	// For temp dirs, fetch and checkout existing branch
	if o.workspacePath(repo, branchName) == "" {
		if err := o.git.FetchAndCheckout(ctx, workDir, branchName); err != nil {
			slog.Error("fetching existing branch", "error", err, "issue", details.Identifier, "branch", branchName)
			o.store.FailRun(runID, -1, err.Error())
			o.failAndTransition(ctx, details.ID, details.Identifier, stage, "failed to fetch branch: "+err.Error())
			return
		}
	}

	// Build input and fetch cross-stage comments
	input := o.buildInput(details, stage, stateName, labelNames)
	input.WorkDir = workDir
	input.BranchName = branchName

	commentNodes, err := o.client.GetIssueComments(ctx, details.ID)
	if err != nil {
		slog.Warn("fetching cross-stage comments", "error", err, "issue", details.Identifier)
	} else if len(commentNodes) > 0 {
		input.Comments = convertComments(commentNodes)
	}

	result, err := o.runner.Run(ctx, input)
	if err != nil {
		slog.Error("subprocess execution error",
			"error", err,
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.TimeoutRun(runID, err.Error())
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, err.Error())
		return
	}

	switch result.ExitCode {
	case 0:
		// Commit and push (PR auto-updates)
		pushed, err := o.commitAndPush(ctx, workDir, branchName, details, stage.Name)
		if err != nil {
			slog.Error("commit and push failed", "error", err, "issue", details.Identifier)
			o.store.FailRun(runID, -1, err.Error())
			o.failAndTransition(ctx, details.ID, details.Identifier, stage, "subprocess succeeded but push failed: "+err.Error())
			return
		}
		if pushed && prURL != "" {
			o.commentOnPR(ctx, workDir, prURL, stage.Name, details.Identifier)
		}

		slog.Info("subprocess succeeded",
			"issue", details.Identifier,
			"stage", stage.Name,
			"prURL", prURL,
		)
		o.store.CompleteRun(runID, 0, result.Stdout, prURL, branchName)
		if stage.WaitForApproval {
			comment := formatSuccessComment(stage.Name, result.Stdout, prURL)
			if err := o.client.PostComment(ctx, details.ID, comment); err != nil {
				slog.Error("posting comment", "error", err, "issue", details.Identifier)
			}
		} else {
			o.transitionAndComment(ctx, details.ID, details.Identifier, stage, result.Stdout, prURL)
			o.cleanupWorkspaceIfDone(stage, repo, branchName)
		}

	case 2:
		slog.Info("subprocess skipped",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 2, "skipped", prURL, branchName)

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
		o.failAndTransition(ctx, details.ID, details.Identifier, stage, errMsg)
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

// HandleCommentWebhook processes a Comment create webhook for re-runs.
func (o *Orchestrator) HandleCommentWebhook(ctx context.Context, payload linear.WebhookPayload) {
	var comment linear.CommentData
	if err := json.Unmarshal(payload.Data, &comment); err != nil {
		slog.Error("parsing comment data from webhook", "error", err)
		return
	}

	// Loop prevention: ignore ai-flow's own comments
	if strings.HasPrefix(comment.Body, "**ai-flow:") {
		slog.Debug("ignoring own comment", "commentID", comment.ID)
		return
	}

	// Fetch issue details
	details, err := o.client.GetIssue(ctx, comment.IssueID)
	if err != nil {
		slog.Error("fetching issue for comment", "error", err, "issueID", comment.IssueID)
		return
	}

	// Find matching stage for the issue's current state
	stage := o.cfg.FindStage(details.State.Name)
	if stage == nil {
		slog.Debug("no pipeline stage for comment's issue state",
			"state", details.State.Name,
			"issue", details.Identifier,
		)
		return
	}

	// Only re-run if wait_for_approval is enabled
	if !stage.WaitForApproval {
		slog.Debug("ignoring comment on non-wait_for_approval stage",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		return
	}

	// Collect label names and check label filters
	var labelNames []string
	for _, l := range details.Labels.Nodes {
		labelNames = append(labelNames, l.Name)
	}
	if !matchesLabels(stage.Labels, labelNames) {
		slog.Debug("issue does not match label filter for comment re-run",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		return
	}

	// Dedup check
	runID, inserted, err := o.store.StartRun(details.ID, stage.Name)
	if err != nil {
		slog.Error("dedup check failed for comment re-run", "error", err, "issue", details.Identifier)
		return
	}
	if !inserted {
		slog.Info("run already in progress, skipping comment re-run",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		return
	}

	// Fetch all comments and filter out ai-flow's own
	commentNodes, err := o.client.GetIssueComments(ctx, details.ID)
	if err != nil {
		slog.Error("fetching issue comments", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, "failed to fetch comments: "+err.Error())
		return
	}
	comments := filterComments(commentNodes)

	slog.Info("starting comment re-run",
		"issue", details.Identifier,
		"stage", stage.Name,
		"commentCount", len(comments),
	)

	if (stage.CreatesPR || stage.UsesBranch) && o.git != nil {
		o.handleRerunWithGit(ctx, runID, details, stage, details.State.Name, labelNames, comments)
	} else {
		o.handleRerunWithoutGit(ctx, runID, details, stage, details.State.Name, labelNames, comments)
	}
}

func (o *Orchestrator) handleRerunWithoutGit(ctx context.Context, runID int64, details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string, comments []subprocess.Comment) {
	input := o.buildInput(details, stage, stateName, labelNames)
	input.Comments = comments

	result, err := o.runner.Run(ctx, input)
	if err != nil {
		slog.Error("subprocess execution error (re-run)",
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
		slog.Info("subprocess re-run succeeded",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 0, result.Stdout, "", "")
		outputComment := formatSuccessComment(stage.Name, result.Stdout, "")
		if err := o.client.PostComment(ctx, details.ID, outputComment); err != nil {
			slog.Error("posting comment", "error", err, "issue", details.Identifier)
		}

	case 2:
		slog.Info("subprocess re-run skipped",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 2, "skipped", "", "")

	default:
		slog.Warn("subprocess re-run failed",
			"issue", details.Identifier,
			"stage", stage.Name,
			"exitCode", result.ExitCode,
		)
		errMsg := result.Stderr
		if errMsg == "" {
			errMsg = result.Stdout
		}
		o.store.FailRun(runID, result.ExitCode, errMsg)
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, errMsg)
	}
}

func (o *Orchestrator) handleRerunWithGit(ctx context.Context, runID int64, details *linear.IssueDetails, stage *config.StageConfig, stateName string, labelNames []string, comments []subprocess.Comment) {
	repo, baseBranch, err := resolveRepoConfig(details)
	if err != nil {
		slog.Error("resolving repo config", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, err.Error())
		return
	}

	// For uses_branch stages, look up branch from any previous run (cross-stage)
	// For creates_pr stages, look up from the same stage's previous run
	var prevRun *store.RunInfo
	if stage.UsesBranch {
		prevRun, err = o.store.GetFirstBranchForIssue(details.ID)
	} else {
		prevRun, err = o.store.GetLastCompletedRun(details.ID, stage.Name)
	}
	if err != nil {
		slog.Error("looking up previous run", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		return
	}

	branchName := git.SanitizeBranchName(details.Identifier, details.Title)
	prURL := ""
	isRerun := prevRun != nil && prevRun.BranchName != ""
	if isRerun {
		branchName = prevRun.BranchName
		prURL = prevRun.PRURL
	}

	// Set up workspace (persistent or temp)
	workDir, cleanup, err := o.setupWorkspace(ctx, repo, baseBranch, branchName, details.Identifier)
	if err != nil {
		slog.Error("setting up workspace", "error", err, "issue", details.Identifier)
		o.store.FailRun(runID, -1, err.Error())
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "failed to set up workspace: "+err.Error())
		return
	}
	defer cleanup()

	if isRerun {
		// For temp dirs, fetch and checkout existing feature branch
		if o.workspacePath(repo, branchName) == "" {
			if err := o.git.FetchAndCheckout(ctx, workDir, branchName); err != nil {
				slog.Error("fetching existing branch", "error", err, "issue", details.Identifier, "branch", branchName)
				o.store.FailRun(runID, -1, err.Error())
				o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "failed to fetch branch: "+err.Error())
				return
			}
		}
	} else {
		// First run: create new branch
		if err := o.git.CreateBranch(ctx, workDir, branchName); err != nil {
			slog.Error("creating branch", "error", err, "issue", details.Identifier)
			o.store.FailRun(runID, -1, err.Error())
			o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "failed to create branch: "+err.Error())
			return
		}
	}

	// Run subprocess with comments
	input := o.buildInput(details, stage, stateName, labelNames)
	input.WorkDir = workDir
	input.BranchName = branchName
	input.Comments = comments

	result, err := o.runner.Run(ctx, input)
	if err != nil {
		slog.Error("subprocess execution error (re-run)",
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
		if isRerun {
			// Push to existing branch (PR auto-updates)
			pushed, err := o.commitAndPush(ctx, workDir, branchName, details, stage.Name)
			if err != nil {
				slog.Error("commit and push failed (re-run)", "error", err, "issue", details.Identifier)
				o.store.FailRun(runID, -1, err.Error())
				o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "re-run succeeded but push failed: "+err.Error())
				return
			}
			if pushed && prURL != "" {
				o.commentOnPR(ctx, workDir, prURL, stage.Name, details.Identifier)
			}
		} else {
			// First run via comment: create PR
			var err error
			prURL, err = o.commitAndCreatePR(ctx, workDir, branchName, baseBranch, details)
			if err != nil {
				slog.Error("creating PR (comment first run)", "error", err, "issue", details.Identifier)
				o.store.FailRun(runID, -1, err.Error())
				o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, "subprocess succeeded but PR creation failed: "+err.Error())
				return
			}

			// Write branch metadata to issue description
			if prURL != "" {
				newDesc := linear.AppendBranchMetadata(details.Description, branchName, prURL)
				if err := o.client.UpdateIssueDescription(ctx, details.ID, newDesc); err != nil {
					slog.Warn("updating issue description with branch metadata", "error", err, "issue", details.Identifier)
				}
			}
		}

		slog.Info("subprocess re-run succeeded",
			"issue", details.Identifier,
			"stage", stage.Name,
			"prURL", prURL,
		)
		o.store.CompleteRun(runID, 0, result.Stdout, prURL, branchName)
		outputComment := formatSuccessComment(stage.Name, result.Stdout, prURL)
		if err := o.client.PostComment(ctx, details.ID, outputComment); err != nil {
			slog.Error("posting comment", "error", err, "issue", details.Identifier)
		}

	case 2:
		slog.Info("subprocess re-run skipped",
			"issue", details.Identifier,
			"stage", stage.Name,
		)
		o.store.CompleteRun(runID, 2, "skipped", "", branchName)

	default:
		slog.Warn("subprocess re-run failed",
			"issue", details.Identifier,
			"stage", stage.Name,
			"exitCode", result.ExitCode,
		)
		errMsg := result.Stderr
		if errMsg == "" {
			errMsg = result.Stdout
		}
		o.store.FailRun(runID, result.ExitCode, errMsg)
		o.postFailureComment(ctx, details.ID, details.Identifier, stage.Name, errMsg)
	}
}

// commitAndPush commits all changes and pushes to the existing branch (no PR creation).
// Returns true if changes were committed and pushed.
func (o *Orchestrator) commitAndPush(ctx context.Context, dir, branch string, details *linear.IssueDetails, stageName string) (bool, error) {
	hasChanges, err := o.git.HasChanges(ctx, dir)
	if err != nil {
		return false, fmt.Errorf("checking for changes: %w", err)
	}
	if !hasChanges {
		slog.Info("no changes after subprocess", "issue", details.Identifier)
		return false, nil
	}

	commitMsg := fmt.Sprintf("%s: %s\n\nGenerated by ai-flow (stage: %s)", details.Identifier, details.Title, stageName)
	if err := o.git.CommitAll(ctx, dir, commitMsg); err != nil {
		return false, fmt.Errorf("committing changes: %w", err)
	}

	pushCtx, pushCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer pushCancel()
	if err := o.git.Push(pushCtx, dir, branch); err != nil {
		return false, fmt.Errorf("pushing branch: %w", err)
	}

	return true, nil
}

// filterComments converts CommentNodes to subprocess.Comments, skipping ai-flow's own comments.
func filterComments(nodes []linear.CommentNode) []subprocess.Comment {
	var comments []subprocess.Comment
	for _, n := range nodes {
		if strings.HasPrefix(n.Body, "**ai-flow:") {
			continue
		}
		comments = append(comments, subprocess.Comment{
			Author: n.User.Name,
			Body:   n.Body,
		})
	}
	return comments
}

// convertComments converts ALL CommentNodes to subprocess.Comments (no filtering).
// Used for cross-stage context so downstream stages see previous stage outputs.
func convertComments(nodes []linear.CommentNode) []subprocess.Comment {
	var comments []subprocess.Comment
	for _, n := range nodes {
		comments = append(comments, subprocess.Comment{
			Author: n.User.Name,
			Body:   n.Body,
		})
	}
	return comments
}

// commentOnPR posts a summary comment on the GitHub PR after a stage pushes changes.
func (o *Orchestrator) commentOnPR(ctx context.Context, dir, prURL, stageName, identifier string) {
	if o.git == nil {
		return
	}
	body := fmt.Sprintf("**ai-flow: stage `%s` pushed new commits**\n\nIssue: %s", stageName, identifier)
	if err := o.git.CommentOnPR(ctx, dir, prURL, body); err != nil {
		slog.Warn("failed to comment on PR", "error", err, "prURL", prURL, "issue", identifier)
	}
}

// failAndTransition posts a failure comment then transitions to the stage's FailureState.
func (o *Orchestrator) failAndTransition(ctx context.Context, issueID, identifier string, stage *config.StageConfig, errMsg string) {
	o.postFailureComment(ctx, issueID, identifier, stage.Name, errMsg)
	if stage.FailureState == "" {
		return
	}
	failStateID, ok := o.client.ResolveStateID(stage.FailureState)
	if !ok {
		slog.Error("cannot resolve failure state",
			"failureState", stage.FailureState,
			"issue", identifier,
		)
		return
	}
	if err := o.client.UpdateIssueState(ctx, issueID, failStateID); err != nil {
		slog.Error("transitioning issue to failure state",
			"error", err,
			"issue", identifier,
			"failureState", stage.FailureState,
		)
		return
	}
	slog.Info("transitioned issue to failure state",
		"issue", identifier,
		"to", stage.FailureState,
	)
}
