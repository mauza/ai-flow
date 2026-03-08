package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/linear"
	"github.com/mauza/ai-flow/internal/orchestrator"
)

const defaultProjectPollInterval = 60 * time.Second

// ProjectPoller periodically queries Linear for projects with trigger labels.
type ProjectPoller struct {
	cfg      *config.Config
	linear   *linear.Client
	orch     *orchestrator.ProjectOrchestrator
	interval time.Duration
}

// NewProjectPoller creates a new ProjectPoller.
// If the config has a poll interval configured, it reuses it; otherwise defaults to 60s.
func NewProjectPoller(cfg *config.Config, linearClient *linear.Client, orch *orchestrator.ProjectOrchestrator) *ProjectPoller {
	interval := cfg.Linear.ParsedPollInterval
	if interval == 0 {
		interval = defaultProjectPollInterval
	}
	return &ProjectPoller{
		cfg:      cfg,
		linear:   linearClient,
		orch:     orch,
		interval: interval,
	}
}

// Run starts the project polling loop. It polls immediately on start,
// then every interval. It blocks until ctx is cancelled.
func (pp *ProjectPoller) Run(ctx context.Context) {
	slog.Info("project poller starting",
		"interval", pp.interval,
		"stages", len(pp.cfg.ProjectPipeline),
	)

	// Poll immediately on start
	pp.poll(ctx)

	ticker := time.NewTicker(pp.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("project poller stopping")
			return
		case <-ticker.C:
			pp.poll(ctx)
		}
	}
}

// poll queries each project pipeline stage's trigger label and processes matching projects.
func (pp *ProjectPoller) poll(ctx context.Context) {
	for _, stage := range pp.cfg.ProjectPipeline {
		if ctx.Err() != nil {
			return
		}

		projects, err := pp.linear.ListProjectsWithLabel(ctx, stage.Label)
		if err != nil {
			slog.Error("polling projects for stage",
				"stage", stage.Name,
				"label", stage.Label,
				"error", err,
			)
			continue
		}

		if len(projects) > 0 {
			slog.Info("found projects with trigger label",
				"stage", stage.Name,
				"label", stage.Label,
				"count", len(projects),
			)
		}

		stageCopy := stage
		for _, project := range projects {
			projectCopy := project
			go pp.orch.ProcessProject(ctx, projectCopy, stageCopy)
		}
	}
}
