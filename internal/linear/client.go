package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"
)

const apiURL = "https://api.linear.app/graphql"

// Client is a minimal GraphQL client for the Linear API.
type Client struct {
	apiKey     string
	httpClient *http.Client

	mu           sync.RWMutex
	stateCache   map[string]string // name → ID
	reverseCache map[string]string // ID → name
	labelCache   map[string]string // issue label name → ID
	teamID       string            // cached team ID
}

// NewClient creates a new Linear API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:       apiKey,
		httpClient:   &http.Client{},
		stateCache:   make(map[string]string),
		reverseCache: make(map[string]string),
		labelCache:   make(map[string]string),
	}
}

const (
	maxRetries     = 3
	baseRetryDelay = 500 * time.Millisecond
)

func (c *Client) do(ctx context.Context, req GraphQLRequest, result any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := time.Duration(float64(baseRetryDelay) * math.Pow(2, float64(attempt-1)))
			slog.Debug("retrying Linear API request", "attempt", attempt+1, "delay", delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		lastErr = c.doOnce(ctx, body, result)
		if lastErr == nil {
			return nil
		}

		// Don't retry on context cancellation or client errors (4xx except 429)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("Linear API request failed", "attempt", attempt+1, "error", lastErr)
	}
	return fmt.Errorf("after %d attempts: %w", maxRetries, lastErr)
}

func (c *Client) doOnce(ctx context.Context, body []byte, result any) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	if err := json.Unmarshal(respBody, result); err != nil {
		return fmt.Errorf("unmarshaling response: %w", err)
	}

	return nil
}

// LoadWorkflowStates fetches the team's workflow states and populates the cache.
func (c *Client) LoadWorkflowStates(ctx context.Context, teamKey string) error {
	query := `query($teamKey: String!) {
		teams(filter: { key: { eq: $teamKey } }) {
			nodes {
				id
				states {
					nodes {
						id
						name
						type
					}
				}
				labels {
					nodes {
						id
						name
					}
				}
			}
		}
	}`

	var resp GraphQLResponse[struct {
		Teams struct {
			Nodes []struct {
				ID     string `json:"id"`
				States struct {
					Nodes []WorkflowState `json:"nodes"`
				} `json:"states"`
				Labels struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"nodes"`
		} `json:"teams"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"teamKey": teamKey},
	}, &resp)
	if err != nil {
		return fmt.Errorf("loading workflow states: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}
	if len(resp.Data.Teams.Nodes) == 0 {
		return fmt.Errorf("team %q not found", teamKey)
	}

	team := resp.Data.Teams.Nodes[0]

	c.mu.Lock()
	defer c.mu.Unlock()

	c.teamID = team.ID

	for _, s := range team.States.Nodes {
		c.stateCache[s.Name] = s.ID
		c.reverseCache[s.ID] = s.Name
		slog.Info("loaded workflow state", "name", s.Name, "id", s.ID, "type", s.Type)
	}

	for _, l := range team.Labels.Nodes {
		c.labelCache[l.Name] = l.ID
		slog.Debug("loaded issue label", "name", l.Name, "id", l.ID)
	}

	return nil
}

// ResolveStateID returns the state ID for a given state name.
func (c *Client) ResolveStateID(name string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.stateCache[name]
	return id, ok
}

// ResolveStateName returns the state name for a given state ID.
func (c *Client) ResolveStateName(id string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.reverseCache[id]
	return name, ok
}

// GetIssue fetches full issue details by ID.
func (c *Client) GetIssue(ctx context.Context, id string) (*IssueDetails, error) {
	query := `query($id: String!) {
		issue(id: $id) {
			id
			identifier
			title
			description
			url
			state { id name }
			team { id key }
			labels { nodes { id name } }
			project { id name description }
		}
	}`

	var resp GraphQLResponse[struct {
		Issue IssueDetails `json:"issue"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"id": id},
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("getting issue: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}

	return &resp.Data.Issue, nil
}

// GetIssuesByState fetches issues for a team filtered by workflow state name.
// Returns full issue details so no second fetch is needed.
func (c *Client) GetIssuesByState(ctx context.Context, teamKey, stateName string) ([]IssueDetails, error) {
	query := `query($teamKey: String!, $stateName: String!) {
		issues(
			filter: {
				team: { key: { eq: $teamKey } }
				state: { name: { eq: $stateName } }
			}
			first: 50
		) {
			nodes {
				id
				identifier
				title
				description
				url
				state { id name }
				team { id key }
				labels { nodes { id name } }
				project { id name description }
			}
		}
	}`

	var resp GraphQLResponse[struct {
		Issues struct {
			Nodes []IssueDetails `json:"nodes"`
		} `json:"issues"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"teamKey": teamKey, "stateName": stateName},
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("getting issues by state: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}

	issues := resp.Data.Issues.Nodes
	if len(issues) == 50 {
		slog.Warn("GetIssuesByState returned exactly 50 issues, there may be more (pagination not implemented)",
			"teamKey", teamKey,
			"stateName", stateName,
		)
	}

	return issues, nil
}

// UpdateIssueState transitions an issue to a new workflow state.
func (c *Client) UpdateIssueState(ctx context.Context, issueID, stateID string) error {
	query := `mutation($id: String!, $stateId: String!) {
		issueUpdate(id: $id, input: { stateId: $stateId }) {
			success
		}
	}`

	var resp GraphQLResponse[struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"id": issueID, "stateId": stateID},
	}, &resp)
	if err != nil {
		return fmt.Errorf("updating issue state: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("issue update returned success=false")
	}

	return nil
}

// GetIssueComments fetches all comments on an issue, ordered by creation time.
func (c *Client) GetIssueComments(ctx context.Context, issueID string) ([]CommentNode, error) {
	query := `query($id: String!) {
		issue(id: $id) {
			comments(orderBy: createdAt) {
				nodes {
					id
					body
					createdAt
					user { name }
				}
			}
		}
	}`

	var resp GraphQLResponse[struct {
		Issue struct {
			Comments struct {
				Nodes []CommentNode `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"id": issueID},
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("getting issue comments: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}

	return resp.Data.Issue.Comments.Nodes, nil
}

// UpdateIssueDescription updates the description of a Linear issue.
func (c *Client) UpdateIssueDescription(ctx context.Context, issueID, description string) error {
	query := `mutation($id: String!, $description: String!) {
		issueUpdate(id: $id, input: { description: $description }) {
			success
		}
	}`

	var resp GraphQLResponse[struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"id": issueID, "description": description},
	}, &resp)
	if err != nil {
		return fmt.Errorf("updating issue description: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("issue description update returned success=false")
	}

	return nil
}

// PostComment adds a comment to an issue.
func (c *Client) PostComment(ctx context.Context, issueID, body string) error {
	query := `mutation($issueId: String!, $body: String!) {
		commentCreate(input: { issueId: $issueId, body: $body }) {
			success
		}
	}`

	var resp GraphQLResponse[struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"issueId": issueID, "body": body},
	}, &resp)
	if err != nil {
		return fmt.Errorf("creating comment: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}
	if !resp.Data.CommentCreate.Success {
		return fmt.Errorf("comment create returned success=false")
	}

	return nil
}

// TeamID returns the cached team ID (populated after LoadWorkflowStates).
func (c *Client) TeamID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.teamID
}

// ListProjectsWithLabel returns projects that have the given label name.
func (c *Client) ListProjectsWithLabel(ctx context.Context, labelName string) ([]Project, error) {
	query := `query($labelName: String!) {
		projects(
			filter: {
				labels: { some: { name: { eq: $labelName } } }
			}
			first: 50
		) {
			nodes {
				id
				name
				description
				state { name }
				labels { nodes { id name } }
			}
		}
	}`

	var resp GraphQLResponse[struct {
		Projects struct {
			Nodes []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description"`
				State       struct {
					Name string `json:"name"`
				} `json:"state"`
				Labels struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"nodes"`
		} `json:"projects"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"labelName": labelName},
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("listing projects with label: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}

	var projects []Project
	for _, n := range resp.Data.Projects.Nodes {
		p := Project{
			ID:          n.ID,
			Name:        n.Name,
			Description: n.Description,
			State:       n.State.Name,
		}
		for _, l := range n.Labels.Nodes {
			p.Labels = append(p.Labels, ProjectLabel{ID: l.ID, Name: l.Name})
		}
		projects = append(projects, p)
	}
	return projects, nil
}

// GetProjectIssues returns the titles of existing issues in a project.
func (c *Client) GetProjectIssues(ctx context.Context, projectID string) ([]string, error) {
	query := `query($projectId: String!) {
		issues(
			filter: { project: { id: { eq: $projectId } } }
			first: 250
		) {
			nodes { title }
		}
	}`

	var resp GraphQLResponse[struct {
		Issues struct {
			Nodes []struct {
				Title string `json:"title"`
			} `json:"nodes"`
		} `json:"issues"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"projectId": projectID},
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("getting project issues: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}

	var titles []string
	for _, n := range resp.Data.Issues.Nodes {
		titles = append(titles, n.Title)
	}
	return titles, nil
}

// CreateIssue creates a new issue and returns its ID.
func (c *Client) CreateIssue(ctx context.Context, input CreateIssueInput) (string, error) {
	query := `mutation($input: IssueCreateInput!) {
		issueCreate(input: $input) {
			success
			issue { id identifier }
		}
	}`

	issueInput := map[string]any{
		"teamId":    input.TeamID,
		"title":     input.Title,
		"stateId":   input.StateID,
		"priority":  input.Priority,
	}
	if input.ProjectID != "" {
		issueInput["projectId"] = input.ProjectID
	}
	if input.Description != "" {
		issueInput["description"] = input.Description
	}
	if len(input.LabelIDs) > 0 {
		issueInput["labelIds"] = input.LabelIDs
	}

	var resp GraphQLResponse[struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
			} `json:"issue"`
		} `json:"issueCreate"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"input": issueInput},
	}, &resp)
	if err != nil {
		return "", fmt.Errorf("creating issue: %w", err)
	}
	if len(resp.Errors) > 0 {
		return "", fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}
	if !resp.Data.IssueCreate.Success {
		return "", fmt.Errorf("issueCreate returned success=false")
	}

	return resp.Data.IssueCreate.Issue.ID, nil
}

// RemoveProjectLabel removes a label from a project by updating labelIds to exclude it.
func (c *Client) RemoveProjectLabel(ctx context.Context, projectID, labelID string) error {
	query := `mutation($id: String!, $labelId: String!) {
		projectUpdate(id: $id, input: { removedLabelIds: [$labelId] }) {
			success
		}
	}`

	var resp GraphQLResponse[struct {
		ProjectUpdate struct {
			Success bool `json:"success"`
		} `json:"projectUpdate"`
	}]

	err := c.do(ctx, GraphQLRequest{
		Query:     query,
		Variables: map[string]any{"id": projectID, "labelId": labelID},
	}, &resp)
	if err != nil {
		return fmt.Errorf("removing project label: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", resp.Errors[0].Message)
	}
	if !resp.Data.ProjectUpdate.Success {
		return fmt.Errorf("projectUpdate returned success=false")
	}

	return nil
}

// ResolveIssueLabels converts label names to IDs using the cached label map.
// Unknown labels are logged and skipped (best-effort).
func (c *Client) ResolveIssueLabels(labelNames []string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var ids []string
	for _, name := range labelNames {
		if id, ok := c.labelCache[name]; ok {
			ids = append(ids, id)
		} else {
			slog.Warn("issue label not found in cache, skipping", "label", name)
		}
	}
	return ids
}
