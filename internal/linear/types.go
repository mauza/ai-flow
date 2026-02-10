package linear

import "encoding/json"

// WebhookPayload is the top-level structure Linear sends on webhook events.
type WebhookPayload struct {
	Action           string          `json:"action"`
	Type             string          `json:"type"`
	Data             json.RawMessage `json:"data"`
	UpdatedFrom      json.RawMessage `json:"updatedFrom,omitempty"`
	URL              string          `json:"url,omitempty"`
	CreatedAt        string          `json:"createdAt,omitempty"`
	WebhookID        string          `json:"webhookId,omitempty"`
	WebhookTimestamp int64           `json:"webhookTimestamp,omitempty"`
}

// IssueData is the issue object embedded in webhook payloads.
type IssueData struct {
	ID          string   `json:"id"`
	Identifier  string   `json:"identifier"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	StateID     string   `json:"stateId"`
	TeamID      string   `json:"teamId"`
	LabelIDs    []string `json:"labelIds"`
	URL         string   `json:"url"`
}

// UpdatedFromData captures which fields changed in an update.
type UpdatedFromData struct {
	StateID  string `json:"stateId,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// WorkflowState represents a Linear workflow state.
type WorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// IssueDetails is the full issue returned by a GraphQL query.
type IssueDetails struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	State       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"state"`
	Team struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"team"`
	Labels struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

// GraphQLRequest is a generic GraphQL request body.
type GraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// GraphQLResponse is a generic GraphQL response wrapper.
type GraphQLResponse[T any] struct {
	Data   T              `json:"data"`
	Errors []GraphQLError `json:"errors,omitempty"`
}

// GraphQLError represents a single error from the GraphQL API.
type GraphQLError struct {
	Message    string `json:"message"`
	Extensions any    `json:"extensions,omitempty"`
}
