package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"time"

	"openfabrica/events"
)

// MarshalToolResult encodes a tool result without HTML escaping. The output goes
// to the model transcript and DB, never into HTML, and escaping inflates every
// angle bracket by five bytes against the per-tool result byte budget.
func MarshalToolResult(result any) ([]byte, error) {
	if r, ok := result.(*Result); ok && r != nil {
		r.Data = emptyNilSlices(r.Data)
		// Marshal on a copy so the caller's Result keeps its plain map; only the wire
		// form leads with the fields that carry instructions (see priorityMap).
		if m, ok := r.Data.(map[string]any); ok {
			cp := *r
			cp.Data = priorityMap(m)
			result = &cp
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Executor dispatches tool calls through the registry and publishes events.
type Executor struct {
	registry *Registry
}

// NewExecutor creates an Executor backed by the given registry.
func NewExecutor(registry *Registry) *Executor {
	return &Executor{registry: registry}
}

// DefaultToolTimeout is the per-call deadline for a tool that declares no
// MaxDuration of its own.
const DefaultToolTimeout = 2 * time.Minute

// Timeout returns the deadline a call to the named tool runs under: its
// declared MaxDuration, else DefaultToolTimeout.
func (e *Executor) Timeout(name string) time.Duration {
	if def, err := e.registry.Get(name); err == nil && def.MaxDuration > 0 {
		return def.MaxDuration
	}
	return DefaultToolTimeout
}

// Call is one tool invocation against one project.
type Call struct {
	Name      string
	Args      map[string]any
	ProjectID int
	Project   ProjectStore
	Logger    *slog.Logger
	// Decorate, when set, adds caller-specific fields (the engine's
	// ReadTracker/SeedGuides) to the ToolContext before the tool runs.
	Decorate func(*ToolContext)
}

// Invoke is the single entry point every tool caller uses: it applies the
// tool's deadline, shapes the ToolContext through env (so its DB handle is
// bound to that deadline), and runs the tool with panic recovery, hints, and
// events. It returns the structured result for callers that branch on it and
// the JSON the model reads. A non-nil error means the call itself failed
// (panic, timeout, infrastructure error); the result and JSON then carry that
// failure.
func (e *Executor) Invoke(ctx context.Context, env *Env, c Call) (*Result, string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.Timeout(c.Name))
	defer cancel()
	toolCtx := env.ForProject(ctx, c.Project, c.ProjectID, c.Logger)
	if c.Decorate != nil {
		c.Decorate(toolCtx)
	}
	return e.execute(ctx, toolCtx, c.Name, c.Args)
}

func (e *Executor) execute(ctx context.Context, toolCtx *ToolContext, name string, args map[string]any) (*Result, string, error) {
	def, err := e.registry.Get(name)
	if err != nil {
		r := Fail("unknown tool: %s", name)
		out, _ := e.marshalError(r.Error)
		return r, out, nil
	}

	// Shallow-copy ToolContext so we don't mutate the caller's pointer.
	localCtx := *toolCtx
	localCtx.Ctx = ctx

	start := time.Now()

	// Run tool in a goroutine so the context timeout can cancel it.
	type toolResultPair struct {
		result *Result
		err    error
	}
	ch := make(chan toolResultPair, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				// The stack is for the operator's log; the model gets the message only.
				slog.Error("tool panicked", "tool", name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				ch <- toolResultPair{nil, fmt.Errorf("tool %s panicked: %v", name, p)}
			}
		}()
		r, err := def.Execute(&localCtx, args)
		ch <- toolResultPair{r, err}
	}()

	var result *Result
	var execErr error
	select {
	case tr := <-ch:
		result, execErr = tr.result, tr.err
	case <-ctx.Done():
		// ch is buffered, so the tool goroutine's send still succeeds with no
		// receiver and the goroutine exits cleanly once the tool returns. A
		// tool that blocks forever leaks regardless: no signal from here can
		// kill it, so there's nothing to clean up on this side. Wrap with
		// ErrToolTimeout so callers can match via errors.Is rather than parsing
		// the message; ctx.Err() is also preserved through %w on the format.
		execErr = fmt.Errorf("tool %s timed out (%v)%s: %w", name, ctx.Err(), timeoutWriteNote(def, args), ErrToolTimeout)
	}
	duration := time.Since(start)

	// Build event payload.
	payload := map[string]any{
		"tool":        name,
		"args":        args,
		"duration_ms": duration.Milliseconds(),
	}

	if execErr != nil {
		payload["error"] = execErr.Error()
		if toolCtx.Bus != nil {
			toolCtx.Bus.Publish(events.NewEvent(events.EventToolFailed, toolCtx.ProjectID, payload))
		}
		// A handler error reaches the model as the same structured failure a returned
		// tool.Fail does (valid JSON, success:false, the tool's recovery hint).
		failed := FailErr(execErr)
		applyHints(def, args, failed)
		return failed, marshalOrStatic(failed), execErr
	}

	if result == nil {
		result = Ok(nil)
	}

	applyHints(def, args, result)

	payload["success"] = result.Success
	if toolCtx.Bus != nil {
		toolCtx.Bus.Publish(events.NewEvent(events.EventToolExecuted, toolCtx.ProjectID, payload))
	}

	return result, marshalOrStatic(result), nil
}

// marshalOrStatic encodes a result for the model. A tool that returned something
// unmarshallable (cyclic data, an unexported field) falls through to a static error
// payload; re-marshalling the error message risks a second failure on exotic inputs.
func marshalOrStatic(result *Result) string {
	data, err := MarshalToolResult(result)
	if err != nil {
		return staticMarshalError
	}
	return string(data)
}

// applyHints appends the tool's contextual hint to result. A handler may already have
// set its own data["hint"] for the specific call it just served (epEnableQuery does);
// that one is more specific than the action-level Hints func, so it leads and the
// generic note follows rather than overwriting it.
//
// Hints run on failures too, including a handler that returned an error: a recovery
// note ("delete takes one id; use batch_delete for filters") is worth most on the call
// that just failed. A Hints func returns "" for a note that only makes sense after
// success. A failure, or a success whose Data is not an object (a row list), carries
// the note in the top-level hint field.
func applyHints(def *ToolDef, args map[string]any, result *Result) {
	if def.Hints == nil {
		return
	}
	action, _ := args["action"].(string)
	hint := def.Hints(action, args, result)
	if hint == "" {
		return
	}
	dataMap, isMap := result.Data.(map[string]any)
	switch {
	case result.Success && isMap:
		if own, _ := dataMap["hint"].(string); own != "" && own != hint {
			hint = own + " " + hint
		}
		dataMap["hint"] = hint
	case result.Success && result.Data == nil:
		result.Data = map[string]any{"hint": hint}
	default:
		result.Hint = hint
	}
}

// staticMarshalError is returned when a tool result cannot be JSON-encoded.
// Hardcoded so a marshal failure can never trigger a second marshal.
const staticMarshalError = `{"success":false,"error":"tool result not JSON-serializable"}`

// marshalError returns a JSON-encoded error Result.
func (e *Executor) marshalError(msg string) (string, error) {
	r := Fail("%s", msg)
	data, err := json.Marshal(r)
	if err != nil {
		return staticMarshalError, nil
	}
	return string(data), nil
}

// timeoutWriteNote warns that a timed-out WRITE may still land: the tool goroutine is
// not killed (nothing can stop it from here), so the change can commit after the model
// was told the call failed. Retrying blind then duplicates a row or a send. Empty for a
// read-only action, whose retry is harmless.
func timeoutWriteNote(def *ToolDef, args map[string]any) string {
	action, _ := args["action"].(string)
	if meta, ok := def.ActionMeta[action]; ok && meta.ReadOnly {
		return ""
	}
	return " - the call may still complete in the background, so check the current state (project_state, a get or list) before retrying"
}

// emptyNilSlices turns a nil slice - the Data itself, or a value one level inside a
// Data object - into an empty one, so an empty list marshals as [] rather than null. A
// bare null reads to a model as "the call returned nothing / failed", not "zero rows".
func emptyNilSlices(data any) any {
	if data == nil {
		return nil
	}
	if v := reflect.ValueOf(data); v.Kind() == reflect.Slice && v.IsNil() {
		return reflect.MakeSlice(v.Type(), 0, 0).Interface()
	}
	if m, ok := data.(map[string]any); ok {
		for k, val := range m {
			if val == nil {
				continue
			}
			if v := reflect.ValueOf(val); v.Kind() == reflect.Slice && v.IsNil() {
				m[k] = reflect.MakeSlice(v.Type(), 0, 0).Interface()
			}
		}
	}
	return data
}
