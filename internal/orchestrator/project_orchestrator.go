package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/linear"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/subprocess"
)

// ProjectOrchestrator processes Linear projects through the project pipeline.
type ProjectOrchestrator struct {
	cfg    *config.Config
	linear *linear.Client
	store  *store.Store
	runner *subprocess.Runner
}

// NewProjectOrchestrator creates a new ProjectOrchestrator.
func NewProjectOrchestrator(cfg *config.Config, linearClient *linear.Client, store *store.Store, runner *subprocess.Runner) *ProjectOrchestrator {
	return &ProjectOrchestrator{
		cfg:    cfg,
		linear: linearClient,
		store:  store,
		runner: runner,
	}
}

// plannedIssue represents a single issue to be created, as output by the subprocess.
type plannedIssue struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Priority    int      `json:"priority"`
	Labels      []string `json:"labels"`
}

// ProcessProject runs the project pipeline stage for a single project.
// It handles dedup, subprocess execution, issue creation, and label removal.
func (po *ProjectOrchestrator) ProcessProject(ctx context.Context, project linear.Project, stage config.ProjectStageConfig) {
	log := slog.With("project", project.Name, "stage", stage.Name)

	// Concurrent dedup via DB unique index
	runID, err := po.store.StartProjectRun(project.ID, stage.Name)
	if err != nil {
		log.Info("project run already in progress, skipping", "error", err)
		return
	}

	log.Info("starting project pipeline stage")

	if err := po.processProject(ctx, runID, project, stage, log); err != nil {
		log.Error("project pipeline stage failed", "error", err)
		po.store.FailProjectRun(runID, err.Error())
		return
	}

	po.store.CompleteProjectRun(runID)
	log.Info("project pipeline stage completed")
}

func (po *ProjectOrchestrator) processProject(ctx context.Context, runID int64, project linear.Project, stage config.ProjectStageConfig, log *slog.Logger) error {
	// 1. Fetch existing issues for context
	existingTitles, err := po.linear.GetProjectIssues(ctx, project.ID)
	if err != nil {
		log.Warn("fetching existing project issues", "error", err)
		// Non-fatal: continue without existing issue context
		existingTitles = nil
	}

	// 2. Find trigger label ID (needed for removal after success)
	triggerLabelID := ""
	for _, l := range project.Labels {
		if l.Name == stage.Label {
			triggerLabelID = l.ID
			break
		}
	}
	if triggerLabelID == "" {
		return fmt.Errorf("trigger label %q not found on project (may have been removed)", stage.Label)
	}

	// 3. Build subprocess input
	input := subprocess.Input{
		RunID:              runID,
		StageName:          stage.Name,
		Prompt:             stage.Prompt,
		Command:            stage.Command,
		Args:               stage.Args,
		Timeout:            stage.ParsedTimeout(),
		ContextMode:        po.cfg.Subprocess.ContextMode,
		ProjectID:          project.ID,
		ProjectName:        project.Name,
		ProjectDescription: project.Description,
		ProjectState:       project.State,
		TriggerLabel:       stage.Label,
		ExistingIssues:     existingTitles,
	}

	// 4. Run subprocess
	stageCtx, cancel := context.WithTimeout(ctx, stage.ParsedTimeout())
	defer cancel()

	result, err := po.runner.Run(stageCtx, input)
	if err != nil {
		return fmt.Errorf("subprocess execution: %w", err)
	}
	if result.ExitCode != 0 {
		errMsg := result.Stderr
		if errMsg == "" {
			errMsg = result.Stdout
		}
		return fmt.Errorf("subprocess exited %d: %s", result.ExitCode, truncate(errMsg, 500))
	}

	// 5. Parse subprocess stdout as []plannedIssue
	var planned []plannedIssue
	if err := json.Unmarshal([]byte(result.Stdout), &planned); err != nil {
		return fmt.Errorf("parsing subprocess output as JSON: %w\noutput: %s", err, truncate(result.Stdout, 500))
	}
	log.Info("subprocess returned planned issues", "count", len(planned))

	// 6. Resolve next_state → state ID
	stateID, ok := po.linear.ResolveStateID(stage.NextState)
	if !ok {
		return fmt.Errorf("next_state %q not found in Linear workflow states", stage.NextState)
	}

	// 7. Create each planned issue
	teamID := po.linear.TeamID()
	created := 0
	for _, pi := range planned {
		labelIDs := po.linear.ResolveIssueLabels(pi.Labels)

		issueID, err := po.linear.CreateIssue(ctx, linear.CreateIssueInput{
			TeamID:      teamID,
			ProjectID:   project.ID,
			Title:       pi.Title,
			Description: pi.Description,
			StateID:     stateID,
			Priority:    pi.Priority,
			LabelIDs:    labelIDs,
		})
		if err != nil {
			log.Error("creating planned issue", "title", pi.Title, "error", err)
			// Continue creating other issues rather than aborting
			continue
		}
		log.Info("created issue", "title", pi.Title, "id", issueID)
		created++
	}

	log.Info("issues created", "created", created, "planned", len(planned))

	// 8. Remove trigger label from project (consume trigger)
	if err := po.linear.RemoveProjectLabel(ctx, project.ID, triggerLabelID); err != nil {
		// Log but don't fail the run — issues were already created
		log.Error("removing trigger label from project", "label", stage.Label, "error", err)
	} else {
		log.Info("removed trigger label from project", "label", stage.Label)
	}

	return nil
}

// StaleProjectRunMaxAge is the max age for cleaning up stale project runs on startup.
const StaleProjectRunMaxAge = 10 * time.Minute
