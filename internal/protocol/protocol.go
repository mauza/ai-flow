// Package protocol holds the wire types shared by the control plane and the
// node-runner inside each pod.
package protocol

import (
	"github.com/mauza/ai-flow/internal/flow"
)

// Bundle is everything a node pod receives from the token exchange. It is the
// only way configuration reaches a pod: nothing in it is broader than the
// node's own grants.
type Bundle struct {
	RunID  string `json:"run_id"`
	Seq    int    `json:"seq"`
	Node   string `json:"node"`
	Visit  int    `json:"visit"`
	Type   string `json:"type"`
	Grant  string `json:"grant"`   // bearer token for every pod-facing endpoint
	PodURL string `json:"pod_url"` // base URL of the pod-facing API

	Task    Task   `json:"task"`
	Prompt  string `json:"prompt"`  // the node's rendered instructions
	Context string `json:"context"` // rendered context: previous step, task, run state

	Outcomes     []string       `json:"outcomes"`
	Outputs      map[string]any `json:"outputs,omitempty"`
	ResultSchema map[string]any `json:"result_schema"`

	Repo    *RepoAccess         `json:"repo,omitempty"`
	LLM     *LLMAccess          `json:"llm,omitempty"`
	Harness string              `json:"harness,omitempty"`
	Tools   []string            `json:"tools,omitempty"` // built-in harness tools allowed
	MCP     []MCPTool           `json:"mcp,omitempty"`
	Skills  map[string]SkillDir `json:"skills,omitempty"`
	Check   *CheckSpec          `json:"check,omitempty"`

	TimeoutSeconds int `json:"timeout_seconds"`
}

type Task struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	URL        string `json:"url,omitempty"`
	Identifier string `json:"identifier,omitempty"`
}

type RepoAccess struct {
	CloneURL    string `json:"clone_url"` // through the git proxy
	Branch      string `json:"branch"`
	Base        string `json:"base"`
	Write       bool   `json:"write"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	IncludeDiff bool   `json:"include_diff"` // llm nodes: put the branch diff in the prompt
}

type LLMAccess struct {
	BaseURL string          `json:"base_url"` // pod-facing OpenAI-compatible endpoint
	Models  []ModelInfo     `json:"models"`   // primary first, then fallbacks
	Config  *flow.LLMConfig `json:"config"`
}

type ModelInfo struct {
	Name          string `json:"name"` // catalog name; what the pod sends as "model"
	ContextTokens int    `json:"context_tokens,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
}

type MCPTool struct {
	Server      string         `json:"server"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type SkillDir struct {
	Files map[string]string `json:"files"`
}

type CheckSpec struct {
	Run       string            `json:"run"`
	ExitCodes map[string]string `json:"exit_codes"`
}

// Result is what a pod reports when the node finishes.
type Result struct {
	Outcome string         `json:"outcome"`
	Outputs map[string]any `json:"outputs"`
	Summary string         `json:"summary"`
	// Error set means the node failed for infrastructure reasons (no outcome).
	Error   string    `json:"error,omitempty"`
	Commit  string    `json:"commit,omitempty"`
	Diff    *DiffStat `json:"diff,omitempty"`
	LogTail string    `json:"log_tail,omitempty"`
}

type DiffStat struct {
	FilesChanged int `json:"files_changed"`
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
	LinesChanged int `json:"lines_changed"`
}

type Progress struct {
	Text string `json:"text"`
}

// MCPCall asks the control plane to call one granted MCP tool.
type MCPCall struct {
	Server    string         `json:"server"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

type MCPCallResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"is_error"`
}

// LimitError is the error body the LLM proxy returns when a budget is hit.
type LimitError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"` // budget_exceeded | model_not_allowed
	} `json:"error"`
}
