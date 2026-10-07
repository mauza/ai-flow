package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	maxStreamEventBytes  = 1 << 20
	maxStreamOutputBytes = 16 << 20
)

// ReadStream collects a single text choice from an OpenAI-compatible SSE body.
// The caller owns the reader and must tie blocking reads to ctx (for example,
// by using the body of an HTTP request created with that context).
// A clean EOF is accepted only after
// finish_reason=stop; otherwise [DONE] is required. Keep reading after stop for
// final usage and late errors. Never return partial text on failure.
func ReadStream(ctx context.Context, r io.Reader) (string, Usage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxStreamEventBytes)
	var data, output strings.Builder
	var usage Usage
	var event string
	var eventBytes int
	var sawChoice, finished bool

	dispatch := func() (bool, error) {
		if event == "error" {
			return false, fmt.Errorf("stream: upstream error event")
		}
		if data.Len() == 0 {
			return false, nil
		}
		payload := strings.TrimSpace(data.String())
		if payload == "[DONE]" {
			if !sawChoice {
				return false, fmt.Errorf("stream: no choices in response")
			}
			return true, nil
		}
		var chunk struct {
			Choices []struct {
				Index        int                        `json:"index"`
				Delta        map[string]json.RawMessage `json:"delta"`
				FinishReason *string                    `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage          `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Do not include the frame (or decoder error, which may quote it).
			return false, fmt.Errorf("stream: malformed JSON event")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return false, fmt.Errorf("stream: upstream error frame")
		}
		if len(chunk.Choices) == 0 && chunk.Usage == nil {
			return false, fmt.Errorf("stream: event has no choices or usage")
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 || len(chunk.Choices) != 1 {
				return false, fmt.Errorf("stream: expected a single choice at index 0")
			}
			if finished {
				// LiteLLM may attach final usage to an empty choice after stop.
				// Inspect the entire delta so tool calls and other appended data
				// cannot masquerade as an empty text delta.
				if chunk.Usage != nil && choice.Delta != nil && len(choice.Delta) == 0 &&
					(choice.FinishReason == nil || *choice.FinishReason == "stop") {
					continue
				}
				return false, fmt.Errorf("stream: choice after finish reason")
			}
			if choice.Delta == nil {
				return false, fmt.Errorf("stream: choice has no delta")
			}
			sawChoice = true
			var content string
			if raw, ok := choice.Delta["content"]; ok {
				if err := json.Unmarshal(raw, &content); err != nil {
					return false, fmt.Errorf("stream: malformed JSON event")
				}
			}
			if len(content) > maxStreamOutputBytes-output.Len() {
				return false, fmt.Errorf("stream: output exceeds %d bytes", maxStreamOutputBytes)
			}
			output.WriteString(content)
			if choice.FinishReason != nil {
				// length/content_filter/tool_calls cannot supply a complete text reply.
				if *choice.FinishReason != "stop" {
					return false, fmt.Errorf("stream: incomplete text completion (non-stop finish reason)")
				}
				finished = true
			}
		}
		return false, nil
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", Usage{}, err
		}
		line := scanner.Text() // ScanLines handles both LF and CRLF.
		if line == "" {
			done, err := dispatch()
			if err != nil {
				return "", Usage{}, err
			}
			if done {
				return output.String(), usage, nil
			}
			data.Reset()
			event, eventBytes = "", 0
			continue
		}
		eventBytes += len(line) + 1
		if eventBytes > maxStreamEventBytes {
			return "", Usage{}, fmt.Errorf("stream: event exceeds %d bytes", maxStreamEventBytes)
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		case "event":
			event = value
		}
	}
	if err := ctx.Err(); err != nil {
		return "", Usage{}, err
	}
	if err := scanner.Err(); err != nil {
		return "", Usage{}, fmt.Errorf("stream: reading events: %w", err)
	}
	if data.Len() != 0 || event != "" {
		return "", Usage{}, fmt.Errorf("stream: unterminated event")
	}
	if !finished {
		return "", Usage{}, fmt.Errorf("stream: interrupted before completion")
	}
	return output.String(), usage, nil
}
