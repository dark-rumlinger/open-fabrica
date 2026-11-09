package llmredact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/providers/llm"
	"openfabrica/tools/llmredact"

	_ "modernc.org/sqlite"
)

// newUsersProjectDB returns a fully-migrated project DB with a `users` table
// whose `password` column is registered as a secure (hashed) column - the
// shape create_auth requires and the redactor reads from to know which
// manage_data fields are sensitive.
func newUsersProjectDB(t *testing.T) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		 VALUES ('users', '{"email":"TEXT","password":"TEXT"}', '{"password":"hash"}')`,
	); err != nil {
		t.Fatalf("register users table: %v", err)
	}
	return pdb
}

// logEntry pushes one entry through a redactor-equipped DBLLMLogger and returns
// the persisted request_messages, response_tool_calls and response_content.
func logEntry(t *testing.T, pdb *db.ProjectDB, e llm.LLMLogEntry) (reqMsgs, respTC, respContent string) {
	t.Helper()
	logger := llm.NewDBLLMLogger(pdb.Writer(), llm.WithRedactor(llmredact.New(pdb.Reader())))
	logger.LogLLMCall(e)
	logger.Close() // drains the buffer, so the row is written before we read

	if err := pdb.Reader().QueryRow(
		`SELECT COALESCE(request_messages,''), COALESCE(response_tool_calls,''), COALESCE(response_content,'')
		 FROM of_llm_calls WHERE run_id = ? ORDER BY id DESC LIMIT 1`, e.RunID,
	).Scan(&reqMsgs, &respTC, &respContent); err != nil {
		t.Fatalf("read back of_llm_calls: %v", err)
	}
	return reqMsgs, respTC, respContent
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestPasswordNeverReachesLLMLog is the core guarantee: when the build agent
// seeds a user, the plaintext password it typed must not survive anywhere in
// of_llm_calls - not in the introducing tool call, not in the replayed turn,
// not in the model's prose.
func TestPasswordNeverReachesLLMLog(t *testing.T) {
	pdb := newUsersProjectDB(t)
	const password = "hunter2-secret-pw"

	insert := llm.ToolCall{
		ID:   "call_1",
		Name: "manage_data",
		Arguments: mustJSON(t, map[string]any{
			"action":     "insert",
			"table_name": "users",
			"data":       map[string]any{"email": "alice@example.com", "password": password},
		}),
	}
	// request_messages replays the whole turn: the owner's request, the
	// assistant narrating the password, the assistant tool call, and the tool
	// result - the shape a later iteration logs verbatim.
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: "create user alice with password " + password},
		{Role: llm.RoleAssistant, Content: "Creating alice with password " + password + " now.", ToolCalls: []llm.ToolCall{insert}},
		{Role: llm.RoleTool, ToolCallID: "call_1", Content: `{"success":true,"data":{"id":1,"table":"users"}}`},
	}

	reqMsgs, respTC, respContent := logEntry(t, pdb, llm.LLMLogEntry{
		Source:            "engine",
		RunID:             "run-pw",
		Stage:             "build",
		Model:             "test",
		ProviderType:      "test",
		RequestMessages:   mustJSON(t, messages),
		ResponseContent:   "Done - alice's password is " + password + ".",
		ResponseToolCalls: mustJSON(t, []llm.ToolCall{insert}),
	})

	for _, field := range []struct{ name, val string }{
		{"request_messages", reqMsgs},
		{"response_tool_calls", respTC},
		{"response_content", respContent},
	} {
		if strings.Contains(field.val, password) {
			t.Errorf("plaintext password leaked into %s: %s", field.name, field.val)
		}
		if !strings.Contains(field.val, "[redacted]") {
			t.Errorf("%s was not redacted (no token present): %s", field.name, field.val)
		}
	}

	// Non-secure data must be preserved - we redact secrets, not everything.
	if !strings.Contains(reqMsgs, "alice@example.com") {
		t.Errorf("non-secure column was wrongly redacted; request_messages=%s", reqMsgs)
	}
}

// TestSecretStoreValueRedacted covers the manage_secrets store path: if the
// model ever relays a credential through the tool's `value` argument (instead
// of the owner answering it out-of-band), it must not be logged in the clear.
func TestSecretStoreValueRedacted(t *testing.T) {
	pdb := newUsersProjectDB(t)
	const apiKey = "sk_live_0123456789abcdef"

	store := llm.ToolCall{
		ID:        "call_1",
		Name:      "manage_secrets",
		Arguments: mustJSON(t, map[string]any{"action": "store", "name": "STRIPE_KEY", "value": apiKey}),
	}

	_, respTC, _ := logEntry(t, pdb, llm.LLMLogEntry{
		Source:            "engine",
		RunID:             "run-secret",
		Model:             "test",
		ProviderType:      "test",
		ResponseToolCalls: mustJSON(t, []llm.ToolCall{store}),
	})

	if strings.Contains(respTC, apiKey) {
		t.Errorf("secret value leaked into response_tool_calls: %s", respTC)
	}
	if !strings.Contains(respTC, "[redacted]") {
		t.Errorf("secret value not redacted: %s", respTC)
	}
	// The secret name is a harmless reference and should be kept for debugging.
	if !strings.Contains(respTC, "STRIPE_KEY") {
		t.Errorf("secret name was wrongly redacted: %s", respTC)
	}
}

// TestOrdinaryCallUnchanged guards against over-redaction: a call with no
// sensitive arguments must pass through byte-for-byte.
func TestOrdinaryCallUnchanged(t *testing.T) {
	pdb := newUsersProjectDB(t)

	list := llm.ToolCall{
		ID:        "call_1",
		Name:      "manage_data",
		Arguments: mustJSON(t, map[string]any{"action": "select", "table_name": "users"}),
	}
	wantTC := mustJSON(t, []llm.ToolCall{list})
	const content = "Listing the users table now."

	reqMsgs, respTC, respContent := logEntry(t, pdb, llm.LLMLogEntry{
		Source:            "engine",
		RunID:             "run-plain",
		Model:             "test",
		ProviderType:      "test",
		RequestMessages:   "",
		ResponseContent:   content,
		ResponseToolCalls: wantTC,
	})

	if respTC != wantTC {
		t.Errorf("response_tool_calls altered for a non-sensitive call:\n got=%s\nwant=%s", respTC, wantTC)
	}
	if respContent != content {
		t.Errorf("response_content altered: %q", respContent)
	}
	_ = reqMsgs
}
