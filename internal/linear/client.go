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
}

// NewClient creates a new Linear API client.
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:       apiKey,
		httpClient:   &http.Client{},
		stateCache:   make(map[string]string),
		reverseCache: make(map[string]string),
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
				states {
					nodes {
						id
						name
						type
					}
				}
			}
		}
	}`

	var resp GraphQLResponse[struct {
		Teams struct {
			Nodes []struct {
				States struct {
					Nodes []WorkflowState `json:"nodes"`
				} `json:"states"`
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

	states := resp.Data.Teams.Nodes[0].States.Nodes

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range states {
		c.stateCache[s.Name] = s.ID
		c.reverseCache[s.ID] = s.Name
		slog.Info("loaded workflow state", "name", s.Name, "id", s.ID, "type", s.Type)
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
