package engine

// Owner-facing publishing: chat messages, engine.* events, the task-queue
// snapshot. Everything the UI and bots see goes through here. pickPhrase
// variety is allowed ONLY in these owner-facing strings - never in anything
// that reaches a prompt (the byte-stability contract).

import (
	"encoding/json"
	"math/rand"
	"strings"

	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/sqliteretry"
	"openfabrica/internal/stringutil"
	"openfabrica/providers/llm"
)

// pickPhrase returns one of the given phrases at random, for owner-facing
// message variety.
func pickPhrase(phrases ...string) string {
	if len(phrases) == 0 {
		return ""
	}
	return phrases[rand.Intn(len(phrases))]
}

// publishEvent emits one event on the bus with the worker's project id, when a
// bus is configured.
func (w *Worker) publishEvent(t events.EventType, payload map[string]any) {
	if w.deps.Bus != nil {
		w.deps.Bus.Publish(events.NewEvent(t, w.projectID, payload))
	}
}

// tr renders one of the engine's own lines in the language it is speaking to the
// owner: the project's reply language. A line with no translation, or a project with
// no language set, comes out in English. Use it to build a message from parts; a
// whole line passed to publishMessage is translated there.
func (w *Worker) tr(format string, args ...any) string {
	return msgcat.T(w.replyLanguage(), format, args...)
}

// publishMessage sends an owner-visible engine message in the owner's language:
// persisted to of_chat_messages (session "brain" - the UI's chat history key) and
// published on the bus. The message is matched against the catalog as a whole, so
// fixed lines and templated lines are translated and anything else, such as text a
// model wrote, goes out as it is.
func (w *Worker) publishMessage(content string) {
	w.publishVerbatim(msgcat.Translate(w.replyLanguage(), content))
}

// publishVerbatim sends an owner-visible message without translating it. It is for
// text the chat UI recognises by its English wording (the expanded-brief marker),
// text a model wrote, and the go-live summary, whose card the UI translates itself.
func (w *Worker) publishVerbatim(content string) {
	if content == "" {
		return
	}
	w.saveChatMessage(llm.Message{Role: llm.RoleAssistant, Content: content})
	if w.deps.Bus != nil {
		w.deps.Bus.Publish(events.NewEvent(events.EventEngineMessage, w.projectID, map[string]any{
			"session_id": "brain",
			"role":       "assistant",
			"content":    content,
		}))
	}
	if w.run != nil {
		w.runlog.message(w.run.ID, content)
	}
}

// publishLLMCall announces one logged LLM call so live viewers (the bot's status
// card) can show what the model is doing without polling the database.
func (w *Worker) publishLLMCall(e llm.LLMLogEntry, logID int64) {
	if w.deps.Bus == nil {
		return
	}
	payload := map[string]any{
		"log_id":        logID,
		"run_id":        e.RunID,
		"stage":         e.Stage,
		"iteration":     e.Iteration,
		"model":         e.Model,
		"provider":      e.ProviderName,
		"input_tokens":  e.InputTokens,
		"output_tokens": e.OutputTokens,
		"cache_read":    e.CacheReadInputTokens,
		"duration_ms":   e.DurationMs,
		"stop_reason":   e.ResponseStopReason,
		"cache_hit":     e.CacheHit,
	}
	if e.ErrorMessage != "" {
		payload["error"] = e.ErrorMessage
	}
	if e.ResponseToolCalls != "" {
		var calls []llm.ToolCall
		if json.Unmarshal([]byte(e.ResponseToolCalls), &calls) == nil {
			names := make([]string, 0, len(calls))
			for _, c := range calls {
				names = append(names, c.Name)
			}
			payload["tool_calls"] = names
		}
	}
	if usd, ok := llm.CallCostUSD(e.ProviderName, e.Model, e.InputTokens, e.OutputTokens,
		e.CacheReadInputTokens, e.CacheCreationInputTokens); ok && !e.CacheHit {
		payload["cost_usd"] = usd
	}
	w.deps.Bus.Publish(events.NewEvent(events.EventEngineLLMCall, w.projectID, payload))
}

// saveChatMessage persists one message into the owner chat history.
func (w *Worker) saveChatMessage(m llm.Message) {
	toolCalls := ""
	if len(m.ToolCalls) > 0 {
		if b, err := json.Marshal(m.ToolCalls); err == nil {
			toolCalls = string(b)
		}
	}
	if _, err := sqliteretry.Exec(w.pdb.Writer(),
		`INSERT INTO of_chat_messages (session_id, role, content, tool_calls, tool_call_id)
		 VALUES ('brain', ?, ?, ?, ?)`,
		string(m.Role), m.Content, nullIfEmptyStr(toolCalls), nullIfEmptyStr(m.ToolCallID),
	); err != nil {
		w.logger.Debug("chat save failed", "error", err)
	}
}

// publishStageChange announces a stage transition. prev is the stage just
// completed (empty for the entry stage); the UI uses it to mark the prior
// pipeline node done. Stage names are emitted UPPERCASE: the UI keys every
// stage map on EXPAND/PLAN/BUILD/LIVE, so raw lowercase run.Stage would
// silently no-op every consumer.
func (w *Worker) publishStageChange(prev, stage string, prevDurationMs int64) {
	if w.deps.Bus == nil {
		return
	}
	payload := map[string]any{
		"stage": strings.ToUpper(stage),
	}
	if prev != "" {
		payload["prev_stage"] = strings.ToUpper(prev)
		// Elapsed time of the stage that just completed, so the live "stage
		// complete" divider shows a duration without waiting for a reload to
		// reseed it from of_run_log.
		if prevDurationMs > 0 {
			payload["duration_ms"] = prevDurationMs
		}
	}
	w.deps.Bus.Publish(events.NewEvent(events.EventEngineStageChange, w.projectID, payload))
}

// publishStopLanded announces that an owner stop has actually settled; live and
// scheduled stops have no other signal (the phase event covers only build).
// Call it AFTER the stop has taken effect, never on the way in. Bus.Publish runs
// subscribers inline on the worker goroutine; nothing in-process may subscribe
// to this and call back into Engine.Send.
func (w *Worker) publishStopLanded(reason string) {
	w.publishEvent(events.EventEngineStopLanded, map[string]any{"reason": reason})
}

// BuildTaskQueuePayload renders the build-queue snapshot event payload. The
// UI consumes this shape on engine.tasks and from the build-queue endpoint.
func BuildTaskQueuePayload(tasks []Task) map[string]any {
	counts := map[string]int{}
	rows := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		counts[t.Status]++
		rows = append(rows, map[string]any{
			"id":     t.ID,
			"title":  t.Title,
			"status": t.Status,
			"origin": t.Origin,
		})
	}
	return map[string]any{
		"total":   len(tasks),
		"done":    counts[TaskDone],
		"active":  counts[TaskActive],
		"pending": counts[TaskPending],
		"blocked": counts[TaskBlocked],
		"tasks":   rows,
	}
}

// publishTaskQueue pushes the current queue snapshot to the UI.
func (w *Worker) publishTaskQueue() {
	if w.deps.Bus == nil {
		return
	}
	tasks, err := LoadTasks(w.pdb.Reader())
	if err != nil {
		return
	}
	w.deps.Bus.Publish(events.NewEvent(events.EventEngineTasks, w.projectID, BuildTaskQueuePayload(tasks)))
}

// sessionEventsForWorker wires the session loop's UI hooks.
func (w *Worker) sessionEvents(stage string) sessionEvents {
	return sessionEvents{
		thinking: func(active bool) {
			if w.deps.Bus == nil {
				return
			}
			state := "end"
			if active {
				state = "start"
			}
			w.deps.Bus.Publish(events.NewEvent(events.EventEngineThinking, w.projectID, map[string]any{"state": state}))
		},
		assistant: func(content string, toolCalls []llm.ToolCall) {
			hasToolCalls := len(toolCalls) > 0
			// PLAN raw JSON, EXPAND raw prose and the spec audit's JSON are working
			// context, not chat; skip log + bus for their tool-call-free turns (each
			// stage re-emits an owner-facing formatted message itself).
			if (stage == StagePlan || stage == StageExpand || stage == StageReview) && !hasToolCalls {
				return
			}
			if content != "" || hasToolCalls {
				// Persist the tool_calls alongside the text so a history reload can
				// re-pair each call with its stored tool-result row and redraw the
				// same tool card the live SSE stream showed. Without them the reload
				// renderer (history.js) has no assistant tool_call to hang the card
				// on and the results are orphaned.
				w.saveChatMessage(llm.Message{Role: llm.RoleAssistant, Content: content, ToolCalls: toolCalls})
			}
			if w.deps.Bus != nil && content != "" {
				w.deps.Bus.Publish(events.NewEvent(events.EventEngineMessage, w.projectID, map[string]any{
					"session_id": "brain",
					"role":       "assistant",
					"content":    content,
					"stage":      stage,
					"tool_call":  hasToolCalls,
				}))
			}
		},
		toolStart: func(tc llm.ToolCall, args map[string]any) {
			if w.deps.Bus == nil {
				return
			}
			w.deps.Bus.Publish(events.NewEvent(events.EventEngineToolStart, w.projectID, map[string]any{
				"tool": tc.Name, "name": tc.Name, "call_id": tc.ID, "args": args,
			}))
		},
		toolResult: func(tc llm.ToolCall, args map[string]any, result string, success bool) {
			w.saveChatMessage(llm.Message{Role: llm.RoleTool, Content: summarizeToolResult(w.deps.ToolRegistry, tc.Name, result), ToolCallID: tc.ID})
			if w.deps.Bus == nil {
				return
			}
			payload := map[string]any{
				"tool": tc.Name, "name": tc.Name, "call_id": tc.ID, "args": args,
			}
			if def, err := w.deps.ToolRegistry.Get(tc.Name); err == nil && def.FullResultInEvents {
				payload["result"] = result
			} else {
				payload["result"] = stringutil.Truncate(result, 500)
			}
			// A failed tool needs a top-level error so the UI renders the card
			// as failed instead of success; the detail stays in result.
			if !success {
				payload["error"] = toolResultErrorText(result)
			}
			w.deps.Bus.Publish(events.NewEvent(events.EventEngineToolResult, w.projectID, payload))
		},
		progress: func(text string) { w.publishMessage(text) },
	}
}

// toolResultErrorText extracts a human-readable error from a failed tool's
// result JSON, falling back to a generic message when none is present.
func toolResultErrorText(result string) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(result), &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	return "tool call failed"
}

// stageStartLine is the friendly "starting this phase" chat message.
func stageStartLine(stage string) string {
	return pickPhrase(stageInfo(stage).StartLines...)
}

// stageEntryLine is the chat line shown as a stage begins. On a resume that
// carries the owner's just-given answer (peekWakeAnswer, set by autoRecover),
// it reads as picking up rather than a cold start, so answering a question
// never looks like the build threw its work away and started over.
func (w *Worker) stageEntryLine(stage string) string {
	if w.peekWakeAnswer() != "" {
		return resumeAfterAnswerLine()
	}
	return stageStartLine(stage)
}

// resumeAfterAnswerLine is the stage-agnostic line for a resume that folds in
// the owner's answer. Marked for translation like the stage start lines.
func resumeAfterAnswerLine() string {
	return msgcat.Mark("Got your answer, thanks. Picking up right where we left off.")
}
