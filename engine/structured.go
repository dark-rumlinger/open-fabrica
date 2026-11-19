package engine

import (
	"context"
	"fmt"
	"time"

	"openfabrica/providers/llm"
)

// structuredCallOpts configures a single-turn structured JSON LLM call.
type structuredCallOpts struct {
	Stage         string
	System        string
	Messages      []llm.Message
	Schema        *llm.ResponseSchema
	MaxTokens     int
	Timeout       time.Duration
	Temperature   *float64
	Thinking      *bool
	ThinkingStage string
	NoReplayCache bool
	// RequireStructured skips the call before sending if the resolved provider
	// does not support ResponseSchema.
	RequireStructured bool
}

// runStructuredCall executes one single-turn structured JSON LLM call,
// attaching Schema when the resolved provider supports structured output, and
// parses the response with parse.
func runStructuredCall[T any](ctx context.Context, w *Worker, opts structuredCallOpts, parse func(string) (T, error)) (T, SessionResult, error) {
	var zero T
	sess := w.newStageSession(opts.Stage)
	rp, rpErr := sess.resolver.resolve()
	if opts.RequireStructured {
		if rpErr != nil {
			return zero, SessionResult{}, rpErr
		}
		if !supportsStructuredOutput(rp) {
			return zero, SessionResult{}, fmt.Errorf("provider does not support structured output")
		}
	}
	spec := SessionSpec{
		Stage:         opts.Stage,
		System:        opts.System,
		Seed:          opts.Messages,
		MaxIterations: 1,
		MaxTokens:     opts.MaxTokens,
		Temperature:   opts.Temperature,
		Timeout:       opts.Timeout,
		Thinking:      opts.Thinking,
		ThinkingStage: opts.ThinkingStage,
		Policy:        policyPlan,
		NoReplayCache: opts.NoReplayCache,
	}
	if opts.Schema != nil && rpErr == nil && supportsStructuredOutput(rp) {
		spec.ResponseSchema = opts.Schema
	}
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		return zero, res, err
	}
	parsed, err := parse(res.Content)
	if err != nil {
		return zero, res, err
	}
	return parsed, res, nil
}
