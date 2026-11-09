package llm

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// deltaTestDB creates the subset of of_llm_calls the logger writes, with the same column
// names and an autoincrement id so LastInsertId chains the way production does.
func deltaTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_llm_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source TEXT, run_id TEXT, session_id TEXT, stage TEXT, iteration INTEGER,
		model TEXT, provider_type TEXT, provider_name TEXT, request_hash TEXT, cache_hit INTEGER,
		request_messages TEXT, request_system TEXT, request_tools TEXT, request_max_tokens INTEGER,
		response_content TEXT, response_tool_calls TEXT, response_stop_reason TEXT,
		input_tokens INTEGER, output_tokens INTEGER,
		cache_read_input_tokens INTEGER, cache_creation_input_tokens INTEGER,
		stable_prefix_bytes INTEGER, stable_prefix_msgs INTEGER, total_request_bytes INTEGER,
		request_tool_bytes INTEGER, tool_prefix_changed INTEGER,
		duration_ms INTEGER, attempts INTEGER, retry_wait_ms INTEGER, used_fallback INTEGER,
		error_message TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// AppendedRequestMessages returns exactly what a stored row newly carries: the
// whole array for a self-contained row, only the delta msgs for a delta row,
// nil for garbage. Scanners rely on this to see each message at its first,
// pre-compaction appearance.
func TestAppendedRequestMessages(t *testing.T) {
	arr := `[{"role":"user","content":"a"},{"role":"tool","content":"b","tool_call_id":"t1"}]`
	if got := AppendedRequestMessages(arr); len(got) != 2 {
		t.Fatalf("self-contained row: got %d messages, want 2", len(got))
	}
	delta := `{"base":1,"prefix":2,"msgs":[{"role":"tool","content":"c","tool_call_id":"t2"}]}`
	got := AppendedRequestMessages(delta)
	if len(got) != 1 || !strings.Contains(string(got[0]), `"c"`) {
		t.Fatalf("delta row: got %v, want just the appended message", got)
	}
	for _, bad := range []string{"", "not json", "{broken"} {
		if got := AppendedRequestMessages(bad); got != nil {
			t.Errorf("AppendedRequestMessages(%q) = %v, want nil", bad, got)
		}
	}
}

// growingTranscript returns the message array a build session would send on turn n: an
// append-only conversation whose prefix never changes.
func growingTranscript(n int) string {
	msgs := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, Message{
			Role:    RoleUser,
			Content: fmt.Sprintf("turn %d %s", i, strings.Repeat("body ", 200)),
		})
	}
	b, _ := json.Marshal(msgs)
	return string(b)
}

// TestRequestDeltaRoundTrip is the whole point of delta storage: an append-only session
// stores only what each turn added, and every row still reads back as the full request the
// model was sent.
func TestRequestDeltaRoundTrip(t *testing.T) {
	db := deltaTestDB(t)
	logger := NewDBLLMLogger(db)

	const turns = 20
	const system = "SYSTEM PROMPT, byte-stable for the whole session"
	for i := 1; i <= turns; i++ {
		logger.LogLLMCall(LLMLogEntry{
			Source: "engine", SessionID: "sess_1", RunID: "run_1", Stage: "build", Iteration: i - 1,
			Model: "m", ProviderType: "p",
			RequestMessages: growingTranscript(i),
			RequestSystem:   system,
			RequestTools:    `["manage_files"]`,
		})
	}
	logger.Close()

	// Every row must replay to exactly what was handed in.
	rows, err := db.Query(`SELECT id, source, run_id, session_id, COALESCE(request_messages,''),
		COALESCE(request_system,''), COALESCE(request_tools,'') FROM of_llm_calls ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	type row struct {
		id                                    int64
		source, run, session, stored, sys, tl string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.source, &r.run, &r.session, &r.stored, &r.sys, &r.tl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(all) != turns {
		t.Fatalf("wrote %d rows, want %d", len(all), turns)
	}

	storedBytes, wholeBytes := 0, 0
	for i, r := range all {
		want := growingTranscript(i + 1)
		chain := CallChain{Source: r.source, RunID: r.run, SessionID: r.session}
		got, gotSys, gotTools := ReplayRequestMessages(db, r.id, chain, r.stored, r.sys, r.tl)
		if !sameMessages(t, got, want) {
			t.Fatalf("row %d replayed to the wrong transcript", i+1)
		}
		if gotSys != system {
			t.Errorf("row %d replayed system = %q, want the session's", i+1, gotSys)
		}
		if gotTools != `["manage_files"]` {
			t.Errorf("row %d replayed tools = %q", i+1, gotTools)
		}
		storedBytes += len(r.stored) + len(r.sys) + len(r.tl)
		wholeBytes += len(want) + len(system) + len(`["manage_files"]`)
	}

	// The saving is the reason this exists. An append-only session that re-sends the whole
	// conversation every turn stores O(turns^2) bytes without it.
	if storedBytes >= wholeBytes/2 {
		t.Errorf("delta storage kept %d bytes against %d whole; expected well under half",
			storedBytes, wholeBytes)
	}
	t.Logf("stored %d bytes instead of %d over %d turns (%.0f%%)",
		storedBytes, wholeBytes, turns, 100*float64(storedBytes)/float64(wholeBytes))

	// The first row is always self-contained, so an old reader and a truncated chain both
	// still find a parseable array.
	if isDeltaEncoded(all[0].stored) {
		t.Error("the first row of a session must be self-contained")
	}
	if !isDeltaEncoded(all[len(all)-1].stored) {
		t.Error("a later row of an append-only session should have been stored as a delta")
	}
}

// TestRequestDeltaFallsBackWhenHistoryIsRewritten: compaction rewrites older messages in
// place, so the next request shares no prefix with the last one logged. That row must store
// itself whole rather than pointing at a base it does not extend.
func TestRequestDeltaFallsBackWhenHistoryIsRewritten(t *testing.T) {
	db := deltaTestDB(t)
	logger := NewDBLLMLogger(db)

	rewritten, _ := json.Marshal([]Message{{Role: RoleUser, Content: "a completely different opening " + strings.Repeat("x", 2000)}})
	logger.LogLLMCall(LLMLogEntry{Source: "engine", SessionID: "s", RequestMessages: growingTranscript(6)})
	logger.LogLLMCall(LLMLogEntry{Source: "engine", SessionID: "s", RequestMessages: string(rewritten)})
	logger.Close()

	var stored string
	if err := db.QueryRow(`SELECT COALESCE(request_messages,'') FROM of_llm_calls ORDER BY id DESC LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("query: %v", err)
	}
	if isDeltaEncoded(stored) {
		t.Fatal("a request sharing no prefix with the previous row must be stored whole")
	}
	if !sameMessages(t, stored, string(rewritten)) {
		t.Error("the rewritten transcript did not survive intact")
	}
}

// TestRequestDeltaIgnoresSessionlessRows: a row with no session id has no chain to join, so
// it is always self-contained.
func TestRequestDeltaIgnoresSessionlessRows(t *testing.T) {
	db := deltaTestDB(t)
	logger := NewDBLLMLogger(db)
	logger.LogLLMCall(LLMLogEntry{Source: "proxy", RequestMessages: growingTranscript(4)})
	logger.LogLLMCall(LLMLogEntry{Source: "proxy", RequestMessages: growingTranscript(5)})
	logger.Close()

	rows, _ := db.Query(`SELECT COALESCE(request_messages,'') FROM of_llm_calls ORDER BY id`)
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		if isDeltaEncoded(s) {
			t.Fatal("a row with no session id must not be delta-encoded")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

func sameMessages(t *testing.T, got, want string) bool {
	t.Helper()
	var a, b []json.RawMessage
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Errorf("got is not a message array: %v", err)
		return false
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("want is not a message array: %v", err)
	}
	if len(a) != len(b) {
		t.Errorf("message count = %d, want %d", len(a), len(b))
		return false
	}
	for i := range a {
		if string(a[i]) != string(b[i]) {
			t.Errorf("message %d differs", i)
			return false
		}
	}
	return true
}

// TestRequestDeltaChainsAcrossSessionsInOneRun is the warm fix-pass, and the reason the
// chain is keyed on the run rather than the session. A fix-pass mints a fresh session id
// while carrying the whole prior transcript forward, so keying on the session made it store
// a second full copy, which can make that single row a large share of the call log.
// A segment rollover has the same shape and gets the same saving.
func TestRequestDeltaChainsAcrossSessionsInOneRun(t *testing.T) {
	db := deltaTestDB(t)
	logger := NewDBLLMLogger(db)

	const first, second = "sess_a", "sess_b"
	const turns = 8
	for i := 1; i <= turns; i++ {
		logger.LogLLMCall(LLMLogEntry{
			Source: "engine", RunID: "run_1", SessionID: first, Stage: "build",
			RequestMessages: growingTranscript(i),
		})
	}
	// The fix-pass: same run, new session, carrying everything the first session ended with
	// plus the fix instruction.
	carried := growingTranscript(turns + 1)
	logger.LogLLMCall(LLMLogEntry{
		Source: "engine", RunID: "run_1", SessionID: second, Stage: "build",
		RequestMessages: carried,
	})
	logger.Close()

	var id int64
	var src, run, sess, stored string
	if err := db.QueryRow(`SELECT id, source, run_id, session_id, COALESCE(request_messages,'')
		FROM of_llm_calls ORDER BY id DESC LIMIT 1`).Scan(&id, &src, &run, &sess, &stored); err != nil {
		t.Fatalf("query: %v", err)
	}
	if sess != second {
		t.Fatalf("last row session = %q, want the fix-pass session", sess)
	}
	if !isDeltaEncoded(stored) {
		t.Fatalf("the fix-pass row stored %d bytes whole; it continues the run's chain and "+
			"should have stored only what it appended", len(stored))
	}
	if len(stored) >= len(carried)/4 {
		t.Errorf("fix-pass row stored %d of %d bytes; the carried transcript was not deduped",
			len(stored), len(carried))
	}
	// And it must still replay to exactly what was sent.
	chain := CallChain{Source: src, RunID: run, SessionID: sess}
	got, _, _ := ReplayRequestMessages(db, id, chain, stored, "", "")
	if !sameMessages(t, got, carried) {
		t.Error("the fix-pass row did not replay to the transcript it was sent")
	}
}

// TestRequestDeltaKeepsRunsApart: two runs never share a chain, so one run's row can never
// become another's base and a run exported on its own always replays whole.
func TestRequestDeltaKeepsRunsApart(t *testing.T) {
	db := deltaTestDB(t)
	logger := NewDBLLMLogger(db)
	logger.LogLLMCall(LLMLogEntry{Source: "engine", RunID: "run_1", SessionID: "s1", RequestMessages: growingTranscript(6)})
	logger.LogLLMCall(LLMLogEntry{Source: "engine", RunID: "run_2", SessionID: "s2", RequestMessages: growingTranscript(7)})
	logger.Close()

	var stored string
	if err := db.QueryRow(`SELECT COALESCE(request_messages,'') FROM of_llm_calls
		WHERE run_id = 'run_2'`).Scan(&stored); err != nil {
		t.Fatalf("query: %v", err)
	}
	if isDeltaEncoded(stored) {
		t.Fatal("a second run must not chain onto the first: exporting it alone would replay short")
	}
}

// TestRequestDeltaKeepsSourcesApart: a trigger or job call that happens to carry a run id
// must not become an engine call's base, because every reader filters by source.
func TestRequestDeltaKeepsSourcesApart(t *testing.T) {
	db := deltaTestDB(t)
	logger := NewDBLLMLogger(db)
	logger.LogLLMCall(LLMLogEntry{Source: "engine", RunID: "run_1", SessionID: "s1", RequestMessages: growingTranscript(6)})
	logger.LogLLMCall(LLMLogEntry{Source: "trigger", RunID: "run_1", SessionID: "s2", RequestMessages: growingTranscript(7)})
	logger.Close()

	var stored string
	if err := db.QueryRow(`SELECT COALESCE(request_messages,'') FROM of_llm_calls
		WHERE source = 'trigger'`).Scan(&stored); err != nil {
		t.Fatalf("query: %v", err)
	}
	if isDeltaEncoded(stored) {
		t.Fatal("a different source sharing a run must not chain onto the engine's rows")
	}
}

// TestOnLoggedReportsRowIDs: the engine announces each call to live viewers from this
// hook, so it must fire once per stored row with that row's id.
func TestOnLoggedReportsRowIDs(t *testing.T) {
	db := deltaTestDB(t)
	var got []int64
	var models []string
	logger := NewDBLLMLogger(db, WithOnLogged(func(e LLMLogEntry, id int64) {
		got = append(got, id)
		models = append(models, e.Model)
	}))
	logger.LogLLMCall(LLMLogEntry{Source: "engine", SessionID: "s", Model: "a", RequestMessages: growingTranscript(2)})
	logger.LogLLMCall(LLMLogEntry{Source: "chat", Model: "b"})
	logger.Close()

	if len(got) != 2 || got[0] >= got[1] {
		t.Fatalf("row ids = %v, want two increasing ids", got)
	}
	if models[0] != "a" || models[1] != "b" {
		t.Errorf("models = %v", models)
	}
}

func TestCallCostUSDUnknownModel(t *testing.T) {
	if _, ok := CallCostUSD("no-such-provider", "no-such-model", 1000, 1000, 0, 0); ok {
		t.Error("an unpriced model must not report a cost")
	}
}
