package llm

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// LLMLogEntry represents a single LLM API call log record.
type LLMLogEntry struct {
	Source    string
	SessionID string
	// RunID groups calls under one engine pipeline run (of_runs.id). Empty for
	// chat-mode calls.
	RunID string
	// RequestHash is the engine's canonical request fingerprint (the
	// of_llm_cache key). Empty for callers that don't compute it.
	RequestHash string
	// CacheHit marks a call served from of_llm_cache without reaching the
	// provider.
	CacheHit  bool
	Stage     string // plan / build / live; empty for chat-mode calls
	Iteration int
	Model     string
	// ProviderType is the wire protocol; ProviderName is the provider row.
	// Both are recorded because most vendors share one transport, so the
	// type alone cannot tell an OpenRouter call from a Z.ai one.
	ProviderType       string
	ProviderName       string
	RequestMessages    string // JSON
	RequestSystem      string
	RequestTools       string // JSON array of tool names
	RequestMaxTokens   int
	ResponseContent    string
	ResponseToolCalls  string // JSON
	ResponseStopReason string
	InputTokens        int
	OutputTokens       int
	// Cache token breakdown (populated by providers that support prompt caching).
	// InputTokens is the non-cached portion only; total billed input is
	// InputTokens + CacheCreationInputTokens + ~10% * CacheReadInputTokens.
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	// Prefix-stability diagnostics copied from the request (see
	// CompletionRequest). Zero for callers that do not track them.
	StablePrefixBytes int
	StablePrefixMsgs  int
	TotalRequestBytes int
	// RequestToolBytes is the serialized size of the tool schemas, and
	// ToolPrefixChanged marks a call whose tool block differs from the previous
	// call in the same segment. The block rides the cached prefix, so a change
	// re-bills everything at full price; without the flag the spike is invisible.
	RequestToolBytes  int
	ToolPrefixChanged bool
	// DurationMs covers the whole call INCLUDING retry backoff. RetryWaitMs is the
	// sleeping part, so provider time is the difference; Attempts counts provider
	// calls made (1 when nothing retried). UsedFallback marks a call the alternate
	// provider answered.
	DurationMs   int64
	Attempts     int
	RetryWaitMs  int64
	UsedFallback bool
	ErrorMessage string
}

// LLMLogger persists LLM log entries.
type LLMLogger interface {
	LogLLMCall(entry LLMLogEntry)
}

// LoggedProvider wraps a Provider and logs every Complete/Stream call. It is
// a full Provider decorator by design: callers today only use Complete, but
// the pass-through methods stay so it can be dropped in anywhere a Provider
// is expected without losing the logging.
var _ Provider = (*LoggedProvider)(nil)

type LoggedProvider struct {
	inner  Provider
	logger LLMLogger
	// source is the call origin: "engine", "trigger", "job", "function" (the
	// unattended paths that count against the daily budget - see
	// UnattendedSources), "chat" (interactive, excluded), or "proxy" / "agent"
	// (end users hitting a hosted LLM or agent endpoint: the public pool, see
	// PublicSources).
	source      string
	session     string
	stage       *string // pointer so the engine can update the stage label between calls without rewrapping
	iterCounter *int
}

// NewLoggedProvider wraps a provider with per-call logging. stage is a
// pointer because the engine reassigns it between stages within the same
// build, the logger reads whatever it points at when each call fires.
// Pass nil for non-engine callers (chat) where stage doesn't apply.
func NewLoggedProvider(inner Provider, logger LLMLogger, source, session string, stage *string, iterCounter *int) *LoggedProvider {
	return &LoggedProvider{
		inner:       inner,
		logger:      logger,
		source:      source,
		session:     session,
		stage:       stage,
		iterCounter: iterCounter,
	}
}

func (lp *LoggedProvider) Name() string { return lp.inner.Name() }
func (lp *LoggedProvider) Type() string { return lp.inner.Type() }
func (lp *LoggedProvider) Ping(ctx context.Context, model string) error {
	return lp.inner.Ping(ctx, model)
}

// SupportsStructuredOutput forwards the optional capability to the wrapped
// provider. A decorator that answered for itself would silently strip schema
// enforcement from every caller that wraps for logging, since the capability is
// discovered by type assertion and the wrapper is what the caller now holds.
// False when the inner provider does not implement the interface at all.
func (lp *LoggedProvider) SupportsStructuredOutput() bool {
	sop, ok := lp.inner.(StructuredOutputProvider)
	return ok && sop.SupportsStructuredOutput()
}

func (lp *LoggedProvider) Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	resp, err := lp.inner.Complete(ctx, req)
	duration := time.Since(start).Milliseconds()
	RecordProviderCall(lp.inner.Name(), err)

	entry := lp.buildEntry(req, duration)

	if err != nil {
		entry.ErrorMessage = err.Error()
	} else {
		entry.ResponseContent = resp.Content
		entry.ResponseStopReason = resp.StopReason
		entry.InputTokens = resp.Usage.InputTokens
		entry.OutputTokens = resp.Usage.OutputTokens
		entry.CacheReadInputTokens = resp.Usage.CacheReadInputTokens
		entry.CacheCreationInputTokens = resp.Usage.CacheCreationInputTokens
		if len(resp.ToolCalls) > 0 {
			if tcJSON, e := json.Marshal(resp.ToolCalls); e == nil {
				entry.ResponseToolCalls = string(tcJSON)
			}
		}
		entry.ResponseContent = mergeReasoningForLog(resp.Content, resp.ReasoningContent)
	}

	lp.logger.LogLLMCall(entry)
	return resp, err
}

func (lp *LoggedProvider) Stream(ctx context.Context, req CompletionRequest, callback StreamCallback) error {
	start := time.Now()

	var (
		contentBuilder   strings.Builder
		reasoningBuilder strings.Builder
		toolCalls        []ToolCall
		usage            Usage
		streamErr        error
	)

	wrappedCallback := func(chunk StreamChunk) {
		if chunk.Delta != "" {
			contentBuilder.WriteString(chunk.Delta)
		}
		if chunk.Reasoning != "" {
			reasoningBuilder.WriteString(chunk.Reasoning)
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		callback(chunk)
	}

	err := lp.inner.Stream(ctx, req, wrappedCallback)
	duration := time.Since(start).Milliseconds()
	if err == nil && streamErr != nil {
		RecordProviderCall(lp.inner.Name(), streamErr)
	} else {
		RecordProviderCall(lp.inner.Name(), err)
	}

	entry := lp.buildEntry(req, duration)
	entry.ResponseContent = mergeReasoningForLog(contentBuilder.String(), reasoningBuilder.String())
	entry.InputTokens = usage.InputTokens
	entry.OutputTokens = usage.OutputTokens
	entry.CacheReadInputTokens = usage.CacheReadInputTokens
	entry.CacheCreationInputTokens = usage.CacheCreationInputTokens

	if len(toolCalls) > 0 {
		if tcJSON, e := json.Marshal(toolCalls); e == nil {
			entry.ResponseToolCalls = string(tcJSON)
		}
	}

	if err != nil {
		entry.ErrorMessage = err.Error()
	} else if streamErr != nil {
		entry.ErrorMessage = streamErr.Error()
	}

	lp.logger.LogLLMCall(entry)
	return err
}

func (lp *LoggedProvider) buildEntry(req CompletionRequest, durationMs int64) LLMLogEntry {
	// stage and iterCounter are the engine's live cursors. Every other caller
	// (chat, jobs, functions, the LLM proxy) has no stage and some have no
	// iteration either, so a nil pointer reads as the zero value instead of
	// faulting on the first logged call.
	stage := ""
	if lp.stage != nil {
		stage = *lp.stage
	}
	iteration := 0
	if lp.iterCounter != nil {
		iteration = *lp.iterCounter
	}
	entry := LLMLogEntry{
		Source:            lp.source,
		SessionID:         lp.session,
		Stage:             stage,
		Iteration:         iteration,
		Model:             req.Model,
		ProviderType:      lp.inner.Type(),
		ProviderName:      lp.inner.Name(),
		RequestSystem:     req.System,
		RequestMaxTokens:  req.MaxTokens,
		StablePrefixBytes: req.StablePrefixBytes,
		StablePrefixMsgs:  req.StablePrefixMsgs,
		TotalRequestBytes: req.TotalRequestBytes,
		DurationMs:        durationMs,
	}

	if msgJSON, e := json.Marshal(req.Messages); e == nil {
		entry.RequestMessages = string(msgJSON)
	}

	entry.RequestTools = toolNamesJSON(req.Tools)
	return entry
}

// mergeReasoningForLog folds a captured reasoning-channel transcript into the
// of_llm_calls ResponseContent column when there is no surfaced content to
// log on its own. The of_llm_calls schema doesn't carry a separate reasoning
// column, but the data is genuinely useful: it's exactly what was billed
// when an iteration ends with an empty content/tool_calls pair despite
// spending output tokens (the engine session's empty-response path).
// Without this, such an iteration looks like the provider returned
// nothing, when in fact it returned chain-of-thought that would otherwise be
// discarded. Visible content and tool calls take priority; reasoning is
// appended only when content is empty so we never bury the model's actual
// answer behind its thoughts.
func mergeReasoningForLog(content, reasoning string) string {
	if reasoning == "" {
		return content
	}
	if content == "" {
		return "[reasoning_content; no surfaced content emitted]\n" + reasoning
	}
	return content
}

// toolNamesJSON returns a JSON array of just the tool names.
func toolNamesJSON(tools []ToolDef) string {
	if len(tools) == 0 {
		return "[]"
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	data, _ := json.Marshal(names)
	return string(data)
}
