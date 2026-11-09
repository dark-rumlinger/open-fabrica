package engine

// Tool dispatch for the session loop: name gating, argument parsing, the
// guard chain, find_tool interception, execution with panic recovery and
// per-tool deadlines, and result bookkeeping.

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"

	"openfabrica/internal/flowsnap"
	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// fallbackReadOnlyActions classifies action names whose read-only semantics
// are unambiguous, for tools that don't declare ReadOnlyActions.
var fallbackReadOnlyActions = map[string]bool{
	"get":      true,
	"list":     true,
	"describe": true,
	"outline":  true,
}

// FallbackReadOnlyActions exposes the fallback set so startup can pass it to
// tools.Registry.OneLinerWarnings. Returns a copy.
func FallbackReadOnlyActions() map[string]bool {
	out := make(map[string]bool, len(fallbackReadOnlyActions))
	for k, v := range fallbackReadOnlyActions {
		out[k] = v
	}
	return out
}

// isReadOnlyToolCall consults ToolDef.ReadOnlyActions, falling back to the
// name-based set.
func (s *session) isReadOnlyToolCall(tc llm.ToolCall, args map[string]any) bool {
	// find_tool never mutates anything. It is not in the registry, so without
	// this it falls through the action fallback and counts as a write, which
	// resets the read-only streak and inflates the run's write count.
	if tc.Name == tools.FindToolName {
		return true
	}
	action, _ := args["action"].(string)
	// A probe that sends a write method is a write whatever its action's static flag
	// says: test_endpoint / test_url POST, PUT or DELETE change the project's data.
	if tc.Name == "manage_testing" && (action == "test_endpoint" || action == "test_url") {
		if method, _ := args["method"].(string); method != "" {
			if m := strings.ToUpper(strings.TrimSpace(method)); m != "GET" && m != "HEAD" {
				return false
			}
		}
	}
	if def, err := s.deps.ToolRegistry.Get(tc.Name); err == nil && len(def.ReadOnlyActions) > 0 {
		if action == "" {
			return false
		}
		return def.ReadOnlyActions[action]
	}
	if action == "" {
		return false
	}
	return fallbackReadOnlyActions[action]
}

// isFlowScriptVerdict reports a write_flows call that left nothing half-changed: the script
// was saved and replayed red, or the save was refused as a rewrite into diagnostics.
func isFlowScriptVerdict(tc llm.ToolCall, args map[string]any, result string) bool {
	switch tc.Name {
	case "manage_testing":
		if action, _ := args["action"].(string); action != "write_flows" {
			return false
		}
		if tools.ResultCode(result) == "diagnostic_suite" {
			return true
		}
	case "manage_files":
		// An edit of the script routed to its owner carries the owner's result; any other
		// manage_files result has no saved script in it.
	default:
		return false
	}
	var r struct {
		Data struct {
			Saved string `json:"saved"`
		} `json:"data"`
	}
	return json.Unmarshal([]byte(result), &r) == nil && r.Data.Saved != ""
}

// segState is the per-segment loop state shared between session.go and the
// dispatch below.
type segState struct {
	// rp, routeKey and rolloverAt are fixed for the segment: the resolved provider,
	// the provider prompt-cache routing key, and the context size that triggers a
	// rollover. lastContextTokens is the TOTAL prompt size of the previous call
	// (uncached + cache-read + cache-creation), the signal the rollover trigger keys
	// on - NOT InputTokens alone, which a cache eviction spikes without the
	// conversation having actually grown.
	rp                resolvedProvider
	routeKey          string
	rolloverAt        int
	lastContextTokens int

	tr       *Transcript
	toolDefs []llm.ToolDef
	// toolBytes is the serialized size of toolDefs, for the total_request_bytes diagnostic.
	// Cached because the tool set only changes when find_tool appends, and marshalling
	// the whole array every turn is pure waste.
	toolBytes int
	// toolsGen bumps on every tool-set change; sentToolsGen is the value the previous
	// request carried. A difference means this call reserializes the tool block the
	// provider cache keyed on, which re-bills the whole prefix at full price. Tracked
	// so that cost is a logged fact rather than an input-token spike someone has to
	// spot by eye.
	toolsGen     int
	sentToolsGen int
	allowed      map[string]bool
	guards       *GuardState
	reads        *tools.ReadTracker
	snapshot     sessionSnapshot
	served       map[string]bool // replay-cache once-per-segment set
	// stop is the ending this segment has decided on, or nil to keep going.
	// Whoever stops names WHY here, at the point of decision: a bool records only
	// that something stopped, and the reason is then gone for good.
	stop        *ending
	readOnlyRun int

	sessionID    string
	segment      int
	iteration    int
	continuation int
	emptyStreak  int
	// runawayTrimmed is set when this turn's cut-off JSON ended in a runaway whitespace
	// run that was trimmed off, so the continuation prompt can ask for compact output.
	runawayTrimmed bool

	// findQueries are the find_tool searches that matched nothing this segment,
	// so a reworded repeat is answered from the first verdict instead of costing
	// another turn.
	findQueries tools.FindQueryLog

	// writeRejects counts THIS turn's failed mutations (reset per turn). A turn with
	// one is half-applied: the calls that succeeded landed and the rejected ones did
	// not, so a stop here ships a file changed halfway.
	writeRejects int

	// Edit-shape counters (manage_files only): how this segment's file edits
	// split between full-body saves, patches, anchor misses, and no-op patches.
	// The run summary surfaces them as the patch-first guardrail - a rising
	// anchor-miss rate is the documented signal to revert the patch-first
	// steer; a rising no-op rate flags a model authoring problem instead.
	saves, patches, anchorMisses, noops int

	// patchRetryGrants is the number of extra iterations awarded because a turn
	// was spent entirely on a drifted or no-op patch. Capped so a couple of retries
	// do not starve a short budget, without letting a stuck model loop forever.
	patchRetryGrants int

	// asksThisTurn counts the extra pause-requesting calls let through after the
	// turn's first question. Reset at the start of each turn.
	asksThisTurn int
}

// stopWith records why this segment is ending. The first reason wins (it is the one that
// actually happened), with one exception: a pause outranks a reason already recorded -
// a turn that both stalls and asks the owner something must park for the answer rather
// than report the stall and drop the question on the floor.
func (st *segState) stopWith(e ending) {
	if st.stop == nil || (e.outcome == EndPaused && st.stop.outcome != EndPaused) {
		st.stop = &e
	}
}

// repeatFindQuery returns the earlier failed search this one restates, or "" when
// it is new. The logic lives in tools.FindQueryLog, shared with the owner chat.
func (st *segState) repeatFindQuery(q string) string { return st.findQueries.Repeat(q) }

// recordFindQuery remembers a search so a later paraphrase of it is refused.
func (st *segState) recordFindQuery(q string) { st.findQueries.Record(q) }

func (st *segState) rebuildAllowed() {
	st.toolBytes = toolDefsBytes(st.toolDefs)
	st.toolsGen++
	st.allowed = make(map[string]bool, len(st.toolDefs))
	for _, td := range st.toolDefs {
		st.allowed[td.Name] = true
	}
}

// hasTool reports whether name is already in the live tool set, so find_tool
// loads only genuinely new schemas (the tool order must stay stable within a
// segment for the provider cache).
func (st *segState) hasTool(name string) bool {
	for _, td := range st.toolDefs {
		if td.Name == name {
			return true
		}
	}
	return false
}

// dedupeRepeatRejection collapses identical failed results within one turn.
// The first occurrence keeps its full body; repeats become a short stub that
// keeps the code and points at the first call, so a batched slip (the same
// mistake across N parallel calls) does not fill the transcript with copies.
func dedupeRepeatRejection(seen map[string]string, tcID, toolName, payload string) (string, bool) {
	key := toolName + "\x00" + payload
	if firstID, ok := seen[key]; ok {
		return fmt.Sprintf(
			`{"success":false,"code":%q,"error":"identical to the error for tool call %s earlier in this turn; fix that error once, it applies to all these calls"}`,
			tools.ResultCode(payload), firstID), true
	}
	seen[key] = tcID
	return payload, false
}

// executeToolCalls runs one turn's tool calls sequentially. Results append to
// the transcript; a call that ends the segment records why via st.stopWith.
func (s *session) executeToolCalls(ctx context.Context, spec *SessionSpec, st *segState, calls []llm.ToolCall) {
	// Per-turn memory for dedupeRepeatRejection; results are substituted
	// before Append, so sealed history is never rewritten.
	seenRejects := map[string]string{}
	st.writeRejects = 0
	st.asksThisTurn = 0
	for _, tc := range calls {
		// Once a blocking question has been asked this turn, skip the rest:
		// they were queued before the model knew the owner's answer. Each
		// still needs a tool_result to keep the protocol valid. Only a PAUSE
		// skips them; a stop for any other reason lets the batch finish. A
		// further question is the exception: the model asked it before seeing any
		// answer too, and the owner gets the whole batch as one grouped form.
		if st.stop != nil && st.stop.outcome == EndPaused && !s.isBatchableAsk(tc, &st.asksThisTurn) {
			skipped := `{"success":false,"code":"paused_for_owner_answer","error":"skipped: the build paused to wait for the owner's answer to a question asked earlier in this turn. This call did not run; the task resumes once the owner answers."}`
			st.tr.Append(llm.Message{Role: llm.RoleTool, Content: skipped, ToolCallID: tc.ID})
			s.events.toolResult(tc, nil, skipped, false)
			continue
		}

		// find_tool: serviced locally - loads schemas into the live set. Answered off the
		// dispatch path, so it repeats that path's arg repair and result truncation rather
		// than inheriting them.
		if tc.Name == tools.FindToolName {
			findArgs, _ := tools.RepairAndUnmarshalArgs(tc.Arguments)
			s.events.toolStart(tc, findArgs)
			payload := s.truncateToolResult(tc.Name, s.handleFindTool(ctx, spec, st, findArgs))
			st.tr.Append(llm.Message{Role: llm.RoleTool, Content: payload, ToolCallID: tc.ID})
			s.events.toolResult(tc, findArgs, payload, tools.ResultIsSuccess(payload))
			continue
		}

		args, validErr := s.validateAndParse(spec, st, tc)

		// Guard: screen. Stop force-stops the session after this iteration.
		if validErr == "" && spec.Guard != nil {
			if v := spec.Guard.Before(st.guards, spec, tc.Name, args); v != nil {
				if v.Stop {
					st.stopWith(endRepeatStall)
				}
				if v.Block {
					s.logger.Warn("guard blocked tool call", "guard", "repeat", "tool", tc.Name)
					validErr = v.Reply
				}
			}
		}

		result, toolErr := s.dispatchTool(ctx, spec, st, tc, args, validErr)
		ok := toolErr == nil && tools.ResultIsSuccess(result)

		// Edit-shape counters for the run summary (the patch-first guardrail).
		if tc.Name == toolFiles {
			switch action, _ := args["action"].(string); action {
			case "save":
				st.saves++
			case "patch":
				st.patches++
				// Only these two count. The patch_empty_search / patch_missing_replace /
				// patch_anchor_consumed / patch_fuzzy_deletion codes are hunk defects,
				// not anchor drift, so counting them would inflate the revert trigger.
				switch tools.ResultCode(result) {
				case "patch_anchor_not_found":
					st.anchorMisses++
				case "patch_noop_replace":
					st.noops++
				}
			}
		}

		// Guard: observe (only executed calls; a refused call never ran).
		if validErr == "" && spec.Guard != nil {
			spec.Guard.After(st.guards, spec, tc.Name, args, result, ok)
		}

		// A rejected EDIT leaves a file part-way, which is what the closing-turn repair
		// prompt is about. A refused checklist update (manage_tasks complete without its
		// deliverables, a blocked dependency) edits nothing, so it must not trigger it.
		if !ok && tc.Name != toolTasks && !s.isReadOnlyToolCall(tc, args) && !isFlowScriptVerdict(tc, args, result) {
			st.writeRejects++
		}

		payload := s.truncateToolResult(tc.Name, result)
		display := result
		if !ok {
			if stub, stubbed := dedupeRepeatRejection(seenRejects, tc.ID, tc.Name, payload); stubbed {
				payload = stub
				display = stub
			}
		}
		st.tr.Append(llm.Message{Role: llm.RoleTool, Content: payload, ToolCallID: tc.ID})
		s.events.toolResult(tc, args, display, ok)

		// Tools declare a pause request via ToolDef.RequestsPause (e.g.
		// manage_chat asking the owner a blocking question).
		if ok {
			if def, err := s.deps.ToolRegistry.Get(tc.Name); err == nil && def.RequestsPause != nil {
				if action, _ := args["action"].(string); def.RequestsPause(action, args) {
					s.logger.Info("tool requested pipeline pause", "tool", tc.Name, "action", action)
					st.stopWith(endToolPause)
				}
			}
		} else if tools.ResultCode(result) == tools.CodeApprovalRequired {
			// A destructive action needs owner approval: the tool already paused
			// the run (of_runs, reason=awaiting_approval) and returned a failed
			// result tagged with the approval code. End the segment cleanly so the
			// worker parks until the owner answers and a wake resumes the run.
			s.logger.Info("tool requested approval pause", "tool", tc.Name)
			st.stopWith(endApproval)
		}
	}
}

// maxAsksPerTurn bounds how many blocking questions one turn can queue.
const maxAsksPerTurn = 6

// isBatchableAsk reports whether tc is another pause-requesting call (a further
// manage_chat ask) that may still run after the turn's first question. The
// first ask already ran by the time this is consulted, so it counts against the
// cap and a turn queues at most maxAsksPerTurn questions. It only counts a call
// it lets through.
func (s *session) isBatchableAsk(tc llm.ToolCall, asked *int) bool {
	def, err := s.deps.ToolRegistry.Get(tc.Name)
	if err != nil || def.RequestsPause == nil {
		return false
	}
	args, _ := tools.RepairAndUnmarshalArgs(tc.Arguments)
	action, _ := args["action"].(string)
	if !def.RequestsPause(action, args) || *asked+1 >= maxAsksPerTurn {
		return false
	}
	*asked++
	return true
}

// validateAndParse gates the tool name and parses arguments. Returns parsed
// args and a non-empty validErr when the call is rejected pre-guard.
func (s *session) validateAndParse(spec *SessionSpec, st *segState, tc llm.ToolCall) (map[string]any, string) {
	switch {
	case st.allowed != nil && !st.allowed[tc.Name]:
		s.logger.Warn("tool not in live set", "tool", tc.Name)
		return nil, s.unavailableToolError(spec, tc.Name)
	case !s.deps.ToolRegistry.Has(tc.Name):
		s.logger.Warn("unknown tool called by LLM", "tool", tc.Name)
		return nil, fmt.Sprintf(`{"error": "unknown tool '%s', check available tool names"}`, tc.Name)
	}

	args, err := tools.RepairAndUnmarshalArgs(tc.Arguments)
	if err != nil {
		s.logger.Warn("bad tool arguments", "tool", tc.Name, "error", err)
		return nil, fmt.Sprintf(`{"error": "invalid JSON arguments for tool %s: %s"}`, tc.Name, err.Error())
	}
	return args, ""
}

// unavailableToolError classifies why a name is not callable so the model
// gets an actionable reason.
func (s *session) unavailableToolError(spec *SessionSpec, name string) string {
	caps, capsErr := llm.LoadCapabilities(s.deps.DB.DB)
	if capsErr != nil {
		s.logger.Warn("unavailableToolError: load capabilities failed", "err", capsErr)
	}
	stage := strings.ToLower(spec.Stage)
	switch s.deps.ToolRegistry.Availability(name, caps, stage) {
	case tools.AvailLoadable:
		return fmt.Sprintf(`{"error": "tool '%s' is not loaded yet; call find_tool(names: [\"%s\"]) to load its schema, then call it"}`, name, name)
	case tools.AvailWrongStage:
		return fmt.Sprintf(`{"error": "tool '%s' is not available during the %s stage; do not call it here"}`, name, stage)
	case tools.AvailMissingCapability:
		return fmt.Sprintf(`{"error": "tool '%s' needs a provider/capability that is not configured for this project; it cannot be used"}`, name)
	default:
		return fmt.Sprintf(`{"error": "unknown tool '%s', check available tool names"}`, name)
	}
}

// hiddenGuides returns the capability guides no loaded provider can back, for the
// guide tool to leave out. Nil when capabilities cannot be read.
func (s *session) hiddenGuides() map[string]bool {
	caps, err := llm.LoadCapabilities(s.deps.DB.DB)
	if err != nil {
		return nil
	}
	ps, err := pack.Embedded()
	if err != nil {
		return nil
	}
	return capabilityDeadGuides(ps, s.deps.ToolRegistry, caps)
}

// dispatchTool executes the tool when validErr is empty, else surfaces the
// rejection as the result. Panic-recovered; runs under the tool's declared
// MaxDuration (see tools.Executor.Invoke).
func (s *session) dispatchTool(ctx context.Context, spec *SessionSpec, st *segState, tc llm.ToolCall, args map[string]any, validErr string) (result string, toolErr error) {
	if validErr != "" {
		return validErr, fmt.Errorf("validation")
	}

	s.events.toolStart(tc, args)

	func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("tool panicked", "tool", tc.Name, "panic", r, "stack", string(debug.Stack()))
				result = toolFailureJSON(fmt.Sprintf("tool %s panicked: %v", tc.Name, r))
				toolErr = fmt.Errorf("panic: %v", r)
			}
		}()

		// test_flows replays of_test.js between a snapshot of the app's data and its restore.
		toolExecCtx := flowsnap.WithOpener(ctx, s.deps.DB.DB, s.pdb, s.deps.ProjectDBManager.ProjectDir(s.projectID), s.projectID)
		_, result, toolErr = s.deps.ToolExecutor.Invoke(toolExecCtx, s.deps.toolEnv(), tools.Call{
			Name:      tc.Name,
			Args:      args,
			ProjectID: s.projectID,
			Project:   s.pdb,
			Logger:    s.logger,
			Decorate: func(tctx *tools.ToolContext) {
				tctx.ReadTracker = st.reads
				tctx.SeedGuides = spec.SeedGuides
				// The flow replay budget keys on the run so it survives the build->repair
				// restart. LIVE and scheduled sessions reuse the finished build's run id,
				// so they get no run key and fall back to the in-memory per-project budget.
				if !strings.EqualFold(spec.Stage, StageLive) {
					tctx.RunID = s.runID
				}
				if tc.Name == "manage_guides" {
					tctx.HiddenGuides = s.hiddenGuides()
				}
			},
		})
		if toolErr != nil {
			// Invoke already returned the structured failure (valid JSON, success:false,
			// the tool's recovery hint) as result; only log it here.
			s.logger.Error("tool failed", "tool", tc.Name, "error", toolErr)
		}
	}()
	return result, toolErr
}

// toolFailureJSON renders a failure the engine itself produced (a panic outside the
// executor) in the same structured shape a tool failure has, with the message escaped.
func toolFailureJSON(msg string) string {
	data, err := tools.MarshalToolResult(tools.Fail("%s", msg))
	if err != nil {
		return `{"success":false,"error":"tool failed"}`
	}
	return string(data)
}

// handleFindTool services a find_tool call: names loads schemas into the live
// set, query keyword-searches the catalogue, neither browses it. raw is the
// already-repaired argument map (see tools.RepairAndUnmarshalArgs). The names/query/
// browse core is shared with live chat via tools.ServiceFindTool; the repeat-
// search guard stays here because it keys on per-segment state.
func (s *session) handleFindTool(ctx context.Context, spec *SessionSpec, st *segState, raw map[string]any) string {
	args := tools.ParseFindToolArgs(raw)

	caps, capsErr := llm.LoadCapabilities(s.deps.DB.DB)
	if capsErr != nil {
		s.logger.Warn("find_tool: load capabilities failed; capability-gated tools hidden", "err", capsErr)
	}

	// A search this segment already ran is answered from the earlier verdict.
	// Rewording the same intent is the shape a fruitless hunt takes, and each
	// round costs a turn plus a full context re-read. A call carrying names is
	// a load, not a search: it must reach the service whatever the query says.
	if len(args.Names) == 0 && strings.TrimSpace(args.Query) != "" {
		if prior := st.repeatFindQuery(args.Query); prior != "" {
			b, _ := json.Marshal(tools.RepeatSearchResult(prior))
			return string(b)
		}
	}

	result, newSchemas := tools.ServiceFindTool(ctx, s.deps.ToolRegistry, caps, strings.ToLower(spec.Stage), args, st.hasTool, s.deps.toolEnv().EmbedFunc)
	if len(newSchemas) > 0 {
		st.toolDefs = append(st.toolDefs, newSchemas...)
		st.rebuildAllowed()
		// A loaded tool stays available for the rest of the run, as the prompts promise:
		// a rollover's fresh segment rebuilds its tool set from spec.PreloadTools, so the
		// load is recorded there too (spec is Run's own copy, never the caller's).
		for _, sc := range newSchemas {
			spec.PreloadTools = append(spec.PreloadTools, sc.Name)
		}
		s.logger.Info("find_tool loaded schemas", "newly_loaded", len(result.Loaded), "already_loaded", len(result.AlreadyLoaded), "live_tools", len(st.toolDefs))
	}
	// Remember a search that matched nothing so a later paraphrase is refused.
	// Skipped when names were given: the service takes the names path and never
	// searches the query, so an empty Matches proves nothing about it.
	if len(args.Names) == 0 && strings.TrimSpace(args.Query) != "" && len(result.Matches) == 0 {
		st.recordFindQuery(args.Query)
	}

	b, _ := json.Marshal(result)
	return string(b)
}

// findToolMaxResult caps find_tool's payload. It is not a registered ToolDef, so it has
// no MaxResult of its own and would otherwise fall to the 3000-byte default - too small for
// a multi-name load, which carries a routing doc per tool. Generous, but bounded: a
// no-argument browse returns the whole stage catalogue, and that is the one call whose size
// scales with the registry rather than with what the model asked for.
const findToolMaxResult = 24000

// truncateToolResult caps a result body for the transcript, honouring the
// tool's declared MaxResult. Delegates to the shared tools helper so chat and
// the engine keep identical caps; find_tool is not a registered ToolDef, so
// its cap stays here.
func (s *session) truncateToolResult(toolName, result string) string {
	if toolName == tools.FindToolName {
		return tools.TruncateResultAt(result, findToolMaxResult)
	}
	return tools.TruncateResult(s.deps.ToolRegistry, toolName, result)
}

// summarizeToolResult condenses a tool result for the chat transcript, using the
// tool's own summarizer when it declares one. A free function over the registry:
// it reads nothing else, so binding it to a session would only oblige callers to
// hold one.
func summarizeToolResult(reg *tools.Registry, toolName, result string) string {
	if def, err := reg.Get(toolName); err == nil && def.Summarize != nil {
		return def.Summarize(result)
	}
	return tools.GenericSummarize(result)
}
