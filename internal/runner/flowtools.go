package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FlowToolsServer is the stdio MCP server harnesses without a pi-style
// extension API load (`ai-flow flow-tools`): flow_finish, and one tool per MCP
// tool granted to the node, called through the control plane. It is the Go
// twin of pi-ext/index.ts and reads the same AI_FLOW_* environment.
func FlowToolsServer() (*mcp.Server, error) {
	bundlePath, resultPath := os.Getenv("AI_FLOW_BUNDLE"), os.Getenv("AI_FLOW_RESULT")
	if bundlePath == "" || resultPath == "" {
		return nil, fmt.Errorf("AI_FLOW_BUNDLE and AI_FLOW_RESULT must be set")
	}
	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, err
	}
	var b toolsBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ai-flow", Version: "1"}, nil)
	addFinish(srv, &b, resultPath)
	grant := os.Getenv("AI_FLOW_GRANT")
	for _, t := range b.MCP {
		addMCPProxy(srv, b.PodURL, grant, t)
	}
	return srv, nil
}

// toolsBundle is the part of the bundle the tools need (bundle.json).
type toolsBundle struct {
	Node         string             `json:"node"`
	Outcomes     []string           `json:"outcomes"`
	ResultSchema map[string]any     `json:"result_schema"`
	MCP          []protocol.MCPTool `json:"mcp"`
	PodURL       string             `json:"pod_url"`
}

func addFinish(srv *mcp.Server, b *toolsBundle, resultPath string) {
	outcomes := slices.DeleteFunc(slices.Clone(b.Outcomes), func(o string) bool { return o == flow.OutcomeLimit || o == flow.OutcomeTimeout })
	outputs := map[string]any{"type": "object", "properties": map[string]any{}}
	if props, ok := b.ResultSchema["properties"].(map[string]any); ok {
		if o, ok := props["outputs"].(map[string]any); ok {
			outputs = o
		}
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"outcome": map[string]any{"type": "string", "enum": outcomes, "description": "One of: " + strings.Join(outcomes, ", ")},
			"summary": map[string]any{"type": "string", "description": "One or two sentences on what you did and why this outcome"},
			"outputs": outputs,
		},
		"required": []string{"outcome", "summary", "outputs"},
	}
	var mu sync.Mutex
	finished := false
	srv.AddTool(&mcp.Tool{
		Name: "flow_finish",
		Description: "Finish this flow step. Report the outcome, a one or two sentence summary, and the step's outputs. " +
			"Call it exactly once, as your final action. Valid outcomes: " + strings.Join(outcomes, ", ") + ".",
		InputSchema: schema,
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Outcome string `json:"outcome"`
			Summary string `json:"summary"`
			Outputs any    `json:"outputs"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return toolError("arguments: " + err.Error()), nil
		}
		if s, ok := in.Outputs.(string); ok {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) != nil {
				return toolError("outputs must be a JSON object, not a string"), nil
			}
			in.Outputs = parsed
		}
		if in.Outputs == nil {
			in.Outputs = map[string]any{}
		}
		if !slices.Contains(outcomes, in.Outcome) {
			return toolError("outcome must be one of: " + strings.Join(outcomes, ", ")), nil
		}
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return toolText("Already finished. Stop now."), nil
		}
		data, err := json.Marshal(map[string]any{"outcome": in.Outcome, "summary": in.Summary, "outputs": in.Outputs})
		if err != nil {
			return toolError(err.Error()), nil
		}
		if err := os.WriteFile(resultPath, data, 0o644); err != nil {
			return toolError(err.Error()), nil
		}
		finished = true
		return toolText(fmt.Sprintf("Recorded outcome %q. The step is complete. Stop now: do not call other tools or write more text.", in.Outcome)), nil
	})
}

func addMCPProxy(srv *mcp.Server, podURL, grant string, t protocol.MCPTool) {
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	desc := t.Description
	if desc == "" {
		desc = fmt.Sprintf("%s (%s)", t.Name, t.Server)
	}
	srv.AddTool(&mcp.Tool{Name: MCPToolName(t.Server, t.Name), Description: desc, InputSchema: schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := json.RawMessage(req.Params.Arguments)
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			body, _ := json.Marshal(map[string]any{"server": t.Server, "tool": t.Name, "arguments": args})
			hreq, err := http.NewRequestWithContext(ctx, "POST", podURL+"/v1/mcp/call", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			hreq.Header.Set("Content-Type", "application/json")
			hreq.Header.Set("Authorization", "Bearer "+grant)
			resp, err := http.DefaultClient.Do(hreq)
			if err != nil {
				return toolError("MCP call failed: " + err.Error()), nil
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode >= 300 {
				return toolError(fmt.Sprintf("MCP call failed (%d): %s", resp.StatusCode, data)), nil
			}
			var out struct {
				Content []map[string]any `json:"content"`
				IsError bool             `json:"is_error"`
			}
			if err := json.Unmarshal(data, &out); err != nil {
				return toolError("MCP call: bad response: " + err.Error()), nil
			}
			var parts []string
			for _, c := range out.Content {
				if c["type"] == "text" {
					parts = append(parts, fmt.Sprint(c["text"]))
				} else {
					j, _ := json.MarshalIndent(c, "", "  ")
					parts = append(parts, string(j))
				}
			}
			res := toolText(strings.Join(parts, "\n"))
			res.IsError = out.IsError
			return res, nil
		})
}

func toolText(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func toolError(s string) *mcp.CallToolResult {
	r := toolText(s)
	r.IsError = true
	return r
}
