package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mauza/ai-flow/internal/llm"
	"slices"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

// runLLM makes one structured completion: no tools, JSON out.
func (r *Runner) runLLM(ctx context.Context) *protocol.Result {
	b := r.b
	if b.LLM == nil || len(b.LLM.Models) == 0 {
		return &protocol.Result{Error: "llm node has no model"}
	}
	shim := NewShim(b.LLM, b.Grant)
	defer shim.Close()

	var user strings.Builder
	if strings.TrimSpace(b.Context) != "" {
		user.WriteString(b.Context)
		user.WriteString("\n\n")
	}
	if b.Repo != nil && b.Repo.IncludeDiff {
		r.progress("Reading the branch diff")
		if d := r.branchDiff(ctx, 60000); d != "" {
			fmt.Fprintf(&user, "## Diff of the branch against %s\n\n```diff\n%s\n```\n\n", b.Repo.Base, d)
		} else {
			fmt.Fprintf(&user, "## Diff of the branch against %s\n\n(no changes yet)\n\n", b.Repo.Base)
		}
	}
	user.WriteString("## Your instructions\n\n")
	user.WriteString(b.Prompt)

	schema, _ := json.MarshalIndent(b.ResultSchema, "", "  ")
	system := fmt.Sprintf(`You are the step %q in an automated flow. Nobody will answer questions.
Reply with a single JSON object and nothing else. It must match this JSON Schema:

%s

"outcome" must be one of: %s.`, b.Node, schema, strings.Join(llmOutcomes(b.Outcomes), ", "))

	messages := []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": user.String()},
	}
	useSchema := true
	transcript := &transcriptCapture{}
	defer func() {
		data, _ := transcript.snapshot(0)
		r.uploadTranscript(data)
	}()
	transcript.event(map[string]any{"type": "llm_request", "messages": messages})

	var lastErr string
	interrupted := 0
	for attempt := 0; attempt < 3; attempt++ {
		r.progress(fmt.Sprintf("Asking %s", shim.CurrentModel()))
		body := map[string]any{"messages": messages, "temperature": 0.2, "stream": true}
		if useSchema {
			body["response_format"] = map[string]any{
				"type":        "json_schema",
				"json_schema": map[string]any{"name": "result", "schema": b.ResultSchema, "strict": true},
			}
		}
		resp, err := shim.Complete(ctx, body)
		if st := shim.StopInfo(); st != nil {
			if st.Action == flow.ActOutcome {
				return &protocol.Result{Outcome: flow.OutcomeLimit, Summary: fmt.Sprintf("Stopped by %s: %s", st.Kind, st.Message), Outputs: map[string]any{"limit": st.Kind}}
			}
			return &protocol.Result{Error: fmt.Sprintf("%s: %s", st.Kind, st.Message)}
		}
		if errors.Is(err, llm.ErrStreamInterrupted) && interrupted < 3 {
			// A control-plane restart cuts streams through the LLM proxy; the
			// proxy is back within a minute. Not the model's fault, so this
			// does not use up a reply attempt.
			interrupted++
			d := streamRetryDelay(interrupted)
			r.progress(fmt.Sprintf("%v; asking again in %s", err, d))
			select {
			case <-ctx.Done():
				return &protocol.Result{Error: err.Error()}
			case <-time.After(d):
			}
			attempt--
			continue
		}
		if err != nil {
			if useSchema && strings.Contains(strings.ToLower(err.Error()), "response_format") {
				useSchema = false
				continue
			}
			return &protocol.Result{Error: err.Error()}
		}
		content := messageContent(resp)
		transcript.event(map[string]any{"type": "llm_response", "model": shim.CurrentModel(), "content": content, "usage": resp["usage"]})
		res, verr := parseResult(content, b.Outcomes)
		if verr == nil {
			return res
		}
		lastErr = verr.Error()
		messages = append(messages,
			map[string]any{"role": "assistant", "content": content},
			map[string]any{"role": "user", "content": "That reply was not valid: " + lastErr + ". Reply again with only the corrected JSON object."})
	}
	return &protocol.Result{Error: "model did not return a valid result: " + lastErr}
}

var streamRetryDelay = func(n int) time.Duration { return time.Duration(n) * 15 * time.Second }

func llmOutcomes(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x != flow.OutcomeLimit && x != flow.OutcomeTimeout {
			out = append(out, x)
		}
	}
	return out
}

func messageContent(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

// parseResult extracts the JSON object from a model reply and checks the outcome.
func parseResult(content string, outcomes []string) (*protocol.Result, error) {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var res protocol.Result
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		return nil, fmt.Errorf("not a JSON object (%v)", err)
	}
	if !slices.Contains(outcomes, res.Outcome) || res.Outcome == flow.OutcomeLimit || res.Outcome == flow.OutcomeTimeout {
		return nil, fmt.Errorf("outcome %q is not one of %s", res.Outcome, strings.Join(llmOutcomes(outcomes), ", "))
	}
	if res.Outputs == nil {
		res.Outputs = map[string]any{}
	}
	return &res, nil
}
