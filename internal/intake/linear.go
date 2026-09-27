// Package intake turns issues from a tracker (Linear) into tasks, and mirrors
// planning and run progress back to the issue.
package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/linear"
	"github.com/mauza/ai-flow/internal/store"
)

type Linear struct {
	cfg *config.Config
	app *app.App
	key string

	mu      sync.Mutex
	clients map[string]*linear.Client // per team key (the client caches one team's states)
	kick    chan struct{}
}

func NewLinear(cfg *config.Config, a *app.App) (*Linear, error) {
	key := config.Secret(cfg.Env.Linear.APIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("linear is enabled but %s is not set", cfg.Env.Linear.APIKeyEnv)
	}
	return &Linear{cfg: cfg, app: a, key: key, clients: map[string]*linear.Client{}, kick: make(chan struct{}, 1)}, nil
}

// WebhookHandler verifies Linear webhooks and triggers an immediate poll, so
// webhook mode reacts in seconds while polling stays the single code path.
func (l *Linear) WebhookHandler() (http.HandlerFunc, error) {
	secret := config.Secret(l.cfg.Env.Linear.WebhookSecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("linear webhook mode needs %s", l.cfg.Env.Linear.WebhookSecretEnv)
	}
	return linear.NewWebhookHandler(secret, func(p linear.WebhookPayload) {
		if p.Type == "Issue" {
			select {
			case l.kick <- struct{}{}:
			default:
			}
		}
	}), nil
}

func (l *Linear) client(ctx context.Context, team string) (*linear.Client, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.clients[team]; ok {
		return c, nil
	}
	c := linear.NewClient(l.key)
	if err := c.LoadWorkflowStates(ctx, team); err != nil {
		return nil, err
	}
	l.clients[team] = c
	return c, nil
}

// Run polls every linked project until ctx ends.
func (l *Linear) Run(ctx context.Context) {
	interval := l.cfg.Env.Linear.PollInterval.Duration
	if l.cfg.Env.Linear.Mode == "webhook" && interval < 5*time.Minute {
		interval = 5 * time.Minute // webhooks trigger polls; this is only a safety net
	}
	slog.Info("linear intake", "mode", l.cfg.Env.Linear.Mode, "poll_every", interval)
	for {
		for name, p := range l.cfg.Projects {
			if p.Spec.Linear == nil {
				continue
			}
			if err := l.poll(ctx, name, p.Spec.Linear); err != nil {
				slog.Warn("linear poll", "project", name, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-l.kick:
		}
	}
}

func (l *Linear) poll(ctx context.Context, project string, link *config.LinearLink) error {
	c, err := l.client(ctx, link.Team)
	if err != nil {
		return err
	}
	for _, state := range link.Trigger.States {
		issues, err := c.GetIssuesByState(ctx, link.Team, state)
		if err != nil {
			return err
		}
		for _, is := range issues {
			if !matches(is, link) {
				continue
			}
			if existing, err := l.app.Store.TaskByExternal(ctx, "linear", is.ID); err == nil {
				// Back in a trigger state after failing: the human wants another try.
				if existing.Status == store.TaskFailed || existing.Status == store.TaskPlanFailed {
					slog.Info("linear: re-planning", "issue", is.Identifier)
					l.app.Store.UpdateTask(ctx, existing.ID, map[string]any{"title": is.Title, "body": is.Description})
					l.app.PlanTask(existing.ID)
				}
				continue
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			t := &store.Task{
				Source: "linear", ExternalID: is.ID, Identifier: is.Identifier,
				Title: is.Title, Body: is.Description, URL: is.URL, Project: project,
			}
			slog.Info("linear: new task", "issue", is.Identifier, "project", project)
			if err := l.app.CreateTask(ctx, t, true); err != nil {
				slog.Error("linear: create task", "issue", is.Identifier, "err", err)
			}
		}
	}
	return nil
}

func matches(is linear.IssueDetails, link *config.LinearLink) bool {
	hasLabel := link.Trigger.Label == ""
	for _, lb := range is.Labels.Nodes {
		if strings.EqualFold(lb.Name, link.Trigger.Label) {
			hasLabel = true
		}
	}
	if !hasLabel {
		return false
	}
	if len(link.Projects) == 0 {
		return true
	}
	if is.Project == nil {
		return false
	}
	for _, p := range link.Projects {
		if strings.EqualFold(p, is.Project.Name) {
			return true
		}
	}
	return false
}

func (l *Linear) link(t *store.Task) *config.LinearLink {
	if t.Source != "linear" || t.ExternalID == "" {
		return nil
	}
	p := l.cfg.Projects[t.Project]
	if p == nil {
		return nil
	}
	return p.Spec.Linear
}

func (l *Linear) move(ctx context.Context, t *store.Task, state string) {
	link := l.link(t)
	if link == nil || state == "" {
		return
	}
	c, err := l.client(ctx, link.Team)
	if err != nil {
		slog.Warn("linear move", "err", err)
		return
	}
	id, ok := c.ResolveStateID(state)
	if !ok {
		slog.Warn("linear: unknown state", "state", state, "team", link.Team)
		return
	}
	if err := c.UpdateIssueState(ctx, t.ExternalID, id); err != nil {
		slog.Warn("linear: move issue", "issue", t.Identifier, "err", err)
	}
}

func (l *Linear) comment(ctx context.Context, t *store.Task, body string) {
	link := l.link(t)
	if link == nil {
		return
	}
	c, err := l.client(ctx, link.Team)
	if err != nil {
		return
	}
	if err := c.PostComment(ctx, t.ExternalID, body); err != nil {
		slog.Warn("linear: comment", "issue", t.Identifier, "err", err)
	}
}

func (l *Linear) url(path string) string {
	return strings.TrimSuffix(l.cfg.Env.Server.PublicURL, "/") + path
}

// ---- app.Notifier ----

func (l *Linear) PlanStarted(ctx context.Context, t *store.Task) {
	if link := l.link(t); link != nil {
		l.move(ctx, t, link.States.Planning)
	}
}

func (l *Linear) PlanFinished(ctx context.Context, t *store.Task, flowName string, valid bool, explanation string) {
	link := l.link(t)
	if link == nil {
		return
	}
	if !link.Comments.Flow {
		if valid {
			l.move(ctx, t, link.States.FlowReady)
		} else {
			l.move(ctx, t, link.States.Failed)
		}
		return
	}
	var sb strings.Builder
	switch {
	case flowName == "":
		fmt.Fprintf(&sb, "**ai-flow** could not plan this task: %s", explanation)
	case valid:
		fmt.Fprintf(&sb, "**ai-flow** planned a flow for this task: [%s](%s)\n\n%s", flowName, l.url("/flows/"+flowName), strings.TrimSpace(explanation))
		if l.startMode(t) == "manual" {
			sb.WriteString("\n\nReview it and press **Run** when it looks right.")
		}
	default:
		fmt.Fprintf(&sb, "**ai-flow** drafted a flow but it still has validation errors: [%s](%s). Fix it in the editor or ask the planner to.", flowName, l.url("/flows/"+flowName))
	}
	l.comment(ctx, t, sb.String())
	if valid {
		l.move(ctx, t, link.States.FlowReady)
	} else {
		l.move(ctx, t, link.States.Failed)
	}
}

func (l *Linear) startMode(t *store.Task) string {
	if p := l.cfg.Projects[t.Project]; p != nil {
		return p.Spec.Start
	}
	return "manual"
}

// ---- engine.Hooks ----

func (l *Linear) RunStarted(ctx context.Context, t *store.Task, r *store.Run) {
	if link := l.link(t); link != nil {
		l.move(ctx, t, link.States.Running)
	}
}

func (l *Linear) RunFinished(ctx context.Context, t *store.Task, r *store.Run) {
	link := l.link(t)
	if link == nil {
		return
	}
	if r.Status == store.RunSucceeded {
		if link.Comments.Result {
			msg := fmt.Sprintf("**ai-flow** finished [run %s](%s).", r.ID, l.url("/runs/"+r.ID))
			if r.PRURL != "" {
				msg += fmt.Sprintf(" Pull request: %s", r.PRURL)
			}
			l.comment(ctx, t, msg)
		}
		l.move(ctx, t, link.States.Succeeded)
		return
	}
	if link.Comments.Result {
		l.comment(ctx, t, fmt.Sprintf("**ai-flow** [run %s](%s) %s: %s", r.ID, l.url("/runs/"+r.ID), r.Status, r.Error))
	}
	l.move(ctx, t, link.States.Failed)
}

// Comment is used by the comment_task action.
func (l *Linear) Comment(ctx context.Context, t *store.Task, body string) error {
	link := l.link(t)
	if link == nil {
		return nil
	}
	c, err := l.client(ctx, link.Team)
	if err != nil {
		return err
	}
	return c.PostComment(ctx, t.ExternalID, body)
}

var _ app.Notifier = (*Linear)(nil)
