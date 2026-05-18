package poller

import (
	"context"
	"log/slog"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/linear"
	"github.com/mauza/ai-flow/internal/orchestrator"
	"github.com/mauza/ai-flow/internal/workqueue"
)

// Poller periodically queries the Linear API for issues in pipeline states.
type Poller struct {
	cfg    *config.Config
	client *linear.Client
	orch   *orchestrator.Orchestrator
	queue  *workqueue.Queue
}

// New creates a new Poller.
func New(cfg *config.Config, client *linear.Client, orch *orchestrator.Orchestrator, queue *workqueue.Queue) *Poller {
	return &Poller{
		cfg:    cfg,
		client: client,
		orch:   orch,
		queue:  queue,
	}
}

// Run starts the polling loop. It polls immediately on start, then every
// poll_interval. It blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	interval := p.cfg.Linear.ParsedPollInterval
	slog.Info("poller starting", "interval", interval, "stages", len(p.cfg.Pipeline))

	// Poll immediately on start
	p.poll(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("poller stopping")
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll queries each pipeline stage's linear_state and processes any matching issues.
func (p *Poller) poll(ctx context.Context) {
	for _, stage := range p.cfg.Pipeline {
		if ctx.Err() != nil {
			return
		}

		issues, err := p.client.GetIssuesByState(ctx, p.cfg.Linear.TeamKey, stage.LinearState)
		if err != nil {
			slog.Error("polling issues for stage",
				"stage", stage.Name,
				"state", stage.LinearState,
				"error", err,
			)
			continue
		}

		if len(issues) > 0 {
			slog.Debug("found issues in state",
				"stage", stage.Name,
				"state", stage.LinearState,
				"count", len(issues),
			)
		}

		stageCopy := stage // capture for goroutine
		for i := range issues {
			issue := issues[i] // capture for queued work
			if err := p.queue.Enqueue(ctx, func(workCtx context.Context) {
				p.orch.ProcessIssue(workCtx, &issue, &stageCopy)
			}); err != nil {
				slog.Error("queueing issue work", "stage", stage.Name, "issue", issue.Identifier, "error", err)
				return
			}
		}
	}
}
