// Package replaymodel stands in for the project's model while a user-flow replay of it runs.
// The app's own model calls are real, slow, billable and different every time, so during a
// replay they are answered here: instantly, free and the same every time. A flow then checks
// the wiring (the reply appears, the loader goes away, the result is shown), not the wording.
// Active exactly while a replay of the project runs, like the replay outbox for mail.
package replaymodel

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"openfabrica/internal/replayoutbox"
	"openfabrica/providers/llm"
)

// Text is the reply every plain-text model call gets during a replay.
const Text = "Test reply from the replay model."

// ModelID is the model name the stand-in reports.
const ModelID = "replay-model"

// ActiveFor reports whether the project's app-facing model calls are answered by the
// stand-in: true exactly while a replay of the project runs.
func ActiveFor(projectID int) bool { return replayoutbox.Active(projectID) }

// Resolve returns the stand-in while a replay of the project runs, and otherwise
// whatever resolve returns. Putting the check before the real lookup also means an
// app whose model is not configured still replays.
func Resolve(projectID int, resolve func() (llm.Provider, string, error)) (llm.Provider, string, error) {
	if ActiveFor(projectID) {
		return New(), ModelID, nil
	}
	return resolve()
}

// Provider answers every completion without calling a model.
type Provider struct{}

// New returns the stand-in provider.
func New() *Provider { return &Provider{} }

func (*Provider) Name() string { return "replay-model" }
func (*Provider) Type() string { return "replay" }

// Ping always succeeds.
func (*Provider) Ping(context.Context, string) error { return nil }

// SupportsStructuredOutput lets callers send a response schema; the stand-in answers
// with a value that satisfies it.
func (*Provider) SupportsStructuredOutput() bool { return true }

// content is the reply for a request: sample JSON when a schema is requested, else Text.
func content(req llm.CompletionRequest) string {
	if req.ResponseSchema != nil && len(req.ResponseSchema.Schema) > 0 {
		return SampleJSON(req.ResponseSchema.Schema)
	}
	return Text
}

// Complete returns the canned reply. It sends no tool calls, so an agent loop ends on
// its first turn.
func (*Provider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &llm.CompletionResponse{Content: content(req), Model: ModelID, StopReason: "end"}, nil
}

// Stream sends the canned reply a word at a time, then the end of the stream.
func (*Provider) Stream(ctx context.Context, req llm.CompletionRequest, callback llm.StreamCallback) error {
	text := content(req)
	words := strings.SplitAfter(text, " ")
	for _, w := range words {
		if err := ctx.Err(); err != nil {
			return err
		}
		if w != "" {
			callback(llm.StreamChunk{Delta: w})
		}
	}
	callback(llm.StreamChunk{Done: true, StopReason: "end", Usage: &llm.Usage{}})
	return nil
}

// SampleJSON returns a small JSON document that satisfies schema: every property of an
// object, one item of an array, the first enum value, and plain defaults otherwise. A
// schema it cannot read yields "{}".
func SampleJSON(schema json.RawMessage) string {
	var node any
	if err := json.Unmarshal(schema, &node); err != nil {
		return "{}"
	}
	out, err := json.Marshal(sample(node, 0))
	if err != nil {
		return "{}"
	}
	return string(out)
}

const maxSampleDepth = 8

// sample builds a value for one schema node.
func sample(node any, depth int) any {
	s, ok := node.(map[string]any)
	if !ok || depth > maxSampleDepth {
		return "sample"
	}
	if v, ok := s["const"]; ok {
		return v
	}
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if alts, ok := s[key].([]any); ok && len(alts) > 0 {
			return sample(alts[0], depth+1)
		}
	}
	switch schemaType(s) {
	case "object":
		props, _ := s["properties"].(map[string]any)
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		obj := make(map[string]any, len(keys))
		for _, k := range keys {
			obj[k] = sample(props[k], depth+1)
		}
		return obj
	case "array":
		n := 1
		if min, ok := s["minItems"].(float64); ok && int(min) > n {
			n = int(min)
		}
		if max, ok := s["maxItems"].(float64); ok && int(max) < n {
			n = int(max)
		}
		items := make([]any, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, sample(s["items"], depth+1))
		}
		return items
	case "integer", "number":
		if min, ok := s["minimum"].(float64); ok {
			return min
		}
		if min, ok := s["exclusiveMinimum"].(float64); ok {
			return min + 1
		}
		return 0
	case "boolean":
		return true
	case "null":
		return nil
	default:
		return sampleString(s)
	}
}

// schemaType is the node's type, taking the first non-null one of a list.
func schemaType(s map[string]any) string {
	switch t := s["type"].(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if str, ok := v.(string); ok && str != "null" {
				return str
			}
		}
	}
	if _, ok := s["properties"]; ok {
		return "object"
	}
	if _, ok := s["items"]; ok {
		return "array"
	}
	return "string"
}

// sampleString is a string for the node's format, padded to its minimum length.
func sampleString(s map[string]any) string {
	v := "sample"
	switch s["format"] {
	case "date-time":
		v = "2026-01-01T00:00:00Z"
	case "date":
		v = "2026-01-01"
	case "email":
		v = "test@example.com"
	case "uri", "url":
		v = "https://example.com"
	case "uuid":
		v = "00000000-0000-4000-8000-000000000000"
	}
	if min, ok := s["minLength"].(float64); ok {
		for len(v) < int(min) {
			v += "x"
		}
	}
	return v
}
