// Package llmredact strips secrets the model itself put into tool-call arguments
// out of LLM log entries before they are persisted to of_llm_calls.
//
// Passwords, encrypted-column values, and stored-secret values reach the platform
// as plaintext tool arguments (manage_data writing a PASSWORD/ENCRYPTED column,
// manage_auth verify_password, manage_secrets store). The tool hashes/encrypts them
// at rest, but the raw argument is what the model emitted, so without scrubbing it
// survives verbatim in of_llm_calls (response_tool_calls on the introducing call,
// request_messages on every replay) and leaks through the build-log export.
//
// Redaction is structural: sensitive fields are identified from the tool name plus,
// for manage_data, the table's secure-column registry. That covers the introducing
// response_tool_calls without depending on whether the tool has run yet (the response
// is logged before the tool runs). A second pass scrubs every value found that way
// from free text (response content and message prose), so a value echoed outside a
// tool argument can't slip through.
//
// Owner-supplied secrets (provider keys, an API key answered to a `secret` question)
// never pass through here: they are encrypted server-side and the model only sees a
// `secret_name` reference, so there is nothing to redact.
package llmredact

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"

	"openfabrica/providers/llm"
	"openfabrica/store"
)

const (
	redactedToken = "[redacted]"
	// minScrubLen guards the free-text pass: only values at least this long are
	// blanked wherever they appear, so a one- or two-character password can't
	// turn the rest of the transcript into swiss cheese by matching common
	// substrings. A shorter value is still redacted structurally inside the tool
	// argument that carried it - only the best-effort prose hunt skips it.
	minScrubLen = 6
)

// Redactor implements llm.LogRedactor for one project, consulting the project's
// secure-column registry to know which manage_data fields are sensitive. Safe
// for concurrent use, though in practice the single logger goroutine drives it.
type Redactor struct {
	db *sql.DB

	mu      sync.Mutex
	secrets map[string]struct{} // distinct plaintext values seen, for the free-text pass
}

// New returns a Redactor backed by db for secure-column lookups. db needs only
// read access (a reader pool is ideal).
func New(db *sql.DB) *Redactor {
	return &Redactor{db: db, secrets: map[string]struct{}{}}
}

// Redact scrubs sensitive tool arguments and any echoes of them from the entry.
func (r *Redactor) Redact(e *llm.LLMLogEntry) {
	if e == nil {
		return
	}
	if e.ResponseToolCalls != "" {
		e.ResponseToolCalls = r.redactToolCalls(e.ResponseToolCalls)
	}
	if e.RequestMessages != "" {
		e.RequestMessages = r.redactMessages(e.RequestMessages)
	}
	// Final pass: blank any discovered secret echoed in the model's own prose.
	if secrets := r.snapshot(); len(secrets) > 0 {
		e.ResponseContent = scrub(e.ResponseContent, secrets)
	}
}

// redactToolCalls rewrites a JSON array of llm.ToolCall, redacting sensitive
// arguments in each. Returns the input unchanged if it isn't that shape.
func (r *Redactor) redactToolCalls(raw string) string {
	var calls []llm.ToolCall
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return raw
	}
	changed := false
	for i := range calls {
		if red, ok := r.redactArgs(calls[i].Name, calls[i].Arguments); ok {
			calls[i].Arguments = red
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(calls)
	if err != nil {
		return raw
	}
	return string(out)
}

// redactMessages rewrites a JSON array of llm.Message: it redacts sensitive
// arguments in every embedded tool call (the replayed turns), then blanks any
// of those values that also surface in message prose or a tool result.
func (r *Redactor) redactMessages(raw string) string {
	var msgs []llm.Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return raw
	}
	changed := false
	// Pass 1: structurally redact tool-call arguments, recording each value.
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			if red, ok := r.redactArgs(msgs[i].ToolCalls[j].Name, msgs[i].ToolCalls[j].Arguments); ok {
				msgs[i].ToolCalls[j].Arguments = red
				changed = true
			}
		}
	}
	// Pass 2: scrub those values where they also appear as free text - the
	// model narrating the secret, or a tool echoing it back in its result.
	if secrets := r.snapshot(); len(secrets) > 0 {
		for i := range msgs {
			if c := scrub(msgs[i].Content, secrets); c != msgs[i].Content {
				msgs[i].Content = c
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(msgs)
	if err != nil {
		return raw
	}
	return string(out)
}

// redactArgs parses one tool call's JSON arguments, blanks the sensitive fields
// for that tool, and re-serializes. The bool reports whether anything changed so
// callers can skip rewriting untouched calls.
func (r *Redactor) redactArgs(tool, argsJSON string) (string, bool) {
	if argsJSON == "" {
		return argsJSON, false
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON, false // not an object payload; nothing structural to do
	}
	changed := false

	// A field literally named "password" is sensitive regardless of tool: this
	// covers manage_auth verify_password and any other caller that takes one.
	if r.blank(args, "password") {
		changed = true
	}

	switch tool {
	case "manage_secrets":
		// store(name, value) - value is the credential material.
		if r.blank(args, "value") {
			changed = true
		}
	case "manage_data":
		// insert/update/upsert carry user values for PASSWORD/ENCRYPTED columns
		// (identified per table by the secure-column registry) and, defensively,
		// any field literally named "password" even on a table that failed to
		// register it as secure. Only look up the registry when a row payload is
		// actually present, so a plain select doesn't trigger a needless query.
		data, hasData := args["data"].(map[string]any)
		rows, hasRows := args["rows"].([]any)
		if hasData || hasRows {
			table, _ := args["table_name"].(string)
			secure := r.secureColumns(table)
			if hasData && r.blankRow(data, secure) {
				changed = true
			}
			for _, rr := range rows {
				if row, ok := rr.(map[string]any); ok {
					if r.blankRow(row, secure) {
						changed = true
					}
				}
			}
		}
	}

	if !changed {
		return argsJSON, false
	}
	out, err := json.Marshal(args)
	if err != nil {
		return argsJSON, false
	}
	return string(out), true
}

// blankRow blanks every secure column present in m, plus any field literally
// named "password" - defense for a table that was never registered secure (in
// which case the value isn't hashed at rest either, but it still must not be
// logged in the clear).
func (r *Redactor) blankRow(m map[string]any, secure map[string]string) bool {
	changed := false
	for col := range secure {
		if r.blank(m, col) {
			changed = true
		}
	}
	if r.blank(m, "password") {
		changed = true
	}
	return changed
}

// blank replaces m[key] with the redaction token and records the prior
// plaintext for the free-text pass. No-op if the key is absent or already
// redacted. Non-string values are still blanked but not recorded (there is no
// stable string form to hunt for in prose).
func (r *Redactor) blank(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	if s, ok := v.(string); ok {
		if s == "" || s == redactedToken {
			return false
		}
		r.note(s)
	}
	m[key] = redactedToken
	return true
}

// note remembers a plaintext value for the free-text pass, ignoring values too
// short to blank safely across prose.
func (r *Redactor) note(s string) {
	if len(s) < minScrubLen {
		return
	}
	r.mu.Lock()
	r.secrets[s] = struct{}{}
	r.mu.Unlock()
}

func (r *Redactor) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.secrets) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.secrets))
	for s := range r.secrets {
		out = append(out, s)
	}
	return out
}

// secureColumns returns the secure-column set for a table (column -> kind), or
// nil when the table is unknown or the lookup fails. The safe direction on any
// error is "redact nothing extra": the structural rules above still cover the
// fields named by tool, and over-redaction is handled by the named rules.
func (r *Redactor) secureColumns(table string) map[string]string {
	if table == "" || r.db == nil {
		return nil
	}
	cols, err := store.LoadSecureColumns(r.db, table)
	if err != nil {
		return nil
	}
	return cols
}

func scrub(s string, secrets []string) string {
	if s == "" {
		return s
	}
	for _, secret := range secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, redactedToken)
		}
	}
	return s
}
