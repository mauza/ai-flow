// Package protocol holds the wire types shared by the control plane and the
// node-runner inside each pod.
package protocol

import (
	"crypto/sha256"
	"fmt"

	"github.com/mauza/ai-flow/internal/flow"
)

const MaxTranscriptBytes = 64 << 20

// TranscriptFinalHeader marks an immutable final snapshot from one runner.
// It does not seal the visit: only acceptance of Result selects its artifact.
// Requests without it are periodic snapshots (also accepted from older runners).
const TranscriptFinalHeader = "X-AI-Flow-Transcript-Final"

// FinalTranscriptKey identifies immutable content within one visit. Retries
// with identical content share a key; different Job attempts can coexist.
func FinalTranscriptKey(runID string, seq int, node string, data []byte) string {
	return fmt.Sprintf("runs/%s/%03d-%s.final-%x.jsonl", runID, seq, node, sha256.Sum256(data))
}

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

	Repo      *RepoAccess         `json:"repo,omitempty"`
	LLM       *LLMAccess          `json:"llm,omitempty"`
	Harness   string              `json:"harness,omitempty"`
	Tools     []string            `json:"tools,omitempty"` // built-in harness tools allowed
	MCP       []MCPTool           `json:"mcp,omitempty"`
	Skills    map[string]SkillDir `json:"skills,omitempty"`
	Agent     *AgentSetup         `json:"agent,omitempty"` // agent nodes: harness and project instructions
	Check     *CheckSpec          `json:"check,omitempty"`
	SecretEnv []string            `json:"secret_env,omitempty"` // approved names only; values are injected by the launcher

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
	Name            string `json:"name"` // catalog name; what the pod sends as "model"
	ContextTokens   int    `json:"context_tokens,omitempty"`
	Reasoning       bool   `json:"reasoning,omitempty"`
	ThinkingFormat  string `json:"thinking_format,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

// AgentSetup configures the harness of an agent node beyond its tools.
type AgentSetup struct {
	Instructions        string         `json:"instructions,omitempty"`         // the harness's, for every project
	ProjectInstructions string         `json:"project_instructions,omitempty"` // the project's agent.instructions
	Settings            map[string]any `json:"settings,omitempty"`             // merged into the harness's settings
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

// CheckSpec is a check node's command. Inputs are the node's rendered inputs;
// the runner exports each as AI_FLOW_INPUT_<NAME> (upper case), so commands can
// use values from earlier steps without templating shell text.
type CheckSpec struct {
	Inputs    map[string]string `json:"inputs,omitempty"`
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
	// Selected atomically with the accepted result, never inferred from the
	// latest upload. Empty selects no artifact (e.g. a failed final upload).
	TranscriptKey string `json:"transcript_key,omitempty"`
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
