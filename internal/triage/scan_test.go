package triage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// toolStep is one assistant tool call plus the tool result it received.
type toolStep struct{ id, name, args, result string }

// scanMini builds a throwaway project DB with just a run and a sequence of tool
// call/result pairs (no llm_calls or run_log), then returns the triage scan. It
// isolates the error-lens scoring - the healthy-catch split and probe-resolution
// recalibration - from the cost lens.
func scanMini(t *testing.T, status string, steps []toolStep) triageBuild {
	t.Helper()
	return scanSeeded(t, seedMini(t, status, steps))
}

// seedMini is scanMini's seeding half, for a test that adds run-log rows first.
func seedMini(t *testing.T, status string, steps []toolStep) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(triageTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO of_runs (id,status,stage,started_at,completed_at)
		 VALUES ('r',?, 'build','2026-01-01T00:00:00Z','2026-01-01T01:00:00Z')`, status); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for i, s := range steps {
		tc := `[{"id":"` + s.id + `","name":"` + s.name + `","arguments":` + jsonStr(s.args) + `}]`
		if _, err := db.Exec(
			`INSERT INTO of_chat_messages (session_id,role,tool_calls,created_at) VALUES ('brain','assistant',?,?)`,
			tc, fmt.Sprintf("2026-01-01T00:%02d:01Z", i+1)); err != nil {
			t.Fatalf("seed asst: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO of_chat_messages (session_id,role,content,tool_call_id,created_at) VALUES ('brain','tool',?,?,?)`,
			s.result, s.id, fmt.Sprintf("2026-01-01T00:%02d:02Z", i+1)); err != nil {
			t.Fatalf("seed tool: %v", err)
		}
	}
	return db
}

func scanSeeded(t *testing.T, db *sql.DB) triageBuild {
	t.Helper()
	runs := listRuns(db, "", 10)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	return scanRun(db, 1, "p", runs[0])
}

// A build whose only tool failures are healthy pre-ship gate catches (bad syntax,
// an undefined reference) is the immune system working - it must read clean, not
// noisy, while the catches stay visible as findings.
func TestTriage_HealthyCatchesAreCleanNotNoisy(t *testing.T) {
	tb := scanMini(t, "completed", []toolStep{
		{"a1", "manage_files", `{"action":"save","file_path":"/files/a.js"}`, `{"success":false,"code":"syntax_error","error":"would introduce a syntax error"}`},
		{"a2", "manage_files", `{"action":"save","file_path":"/files/b.js"}`, `{"success":false,"code":"undefined_reference","error":"uses a name nothing binds"}`},
		{"a4", "manage_files", `{"action":"save","page_path":"/"}`, `{"success":false,"code":"page_not_fragment","error":"page body must be an HTML fragment, found <!DOCTYPE>"}`},
		{"a5", "manage_files", `{"action":"save","kind":"layout","file_path":"default"}`, `{"success":false,"code":"layout_missing_content","error":"layout must contain {{content}} exactly once"}`},
		{"a3", "manage_files", `{"action":"save","file_path":"/files/a.js"}`, `{"success":true}`},
	})
	if tb.Score != 0 {
		t.Errorf("healthy pre-ship catches must not score, got %d", tb.Score)
	}
	if tb.Verdict != "clean" {
		t.Errorf("verdict = %q, want clean", tb.Verdict)
	}
	var labelled bool
	for _, f := range tb.Findings {
		if f.Type == "tool_reject" && strings.Contains(f.Detail, "pre-ship") {
			labelled = true
		}
	}
	if !labelled {
		t.Errorf("healthy catches must still be surfaced (labelled pre-ship): %+v", tb.Findings)
	}
}

// A patch_loop is the SAME rejection repeating on one file. Two DIFFERENT gates
// catching two different defects on consecutive attempts - each fixed on the
// next call - is sequential self-correction, and must not be scored as a loop
// (the shape that inflates a false loop score: an undefined-reference catch then
// an entry-export catch on one entry file, read as a stuck loop).
func TestTriage_DifferentCodesOnOneFileAreNotALoop(t *testing.T) {
	sequential := scanMini(t, "completed", []toolStep{
		{"a1", "manage_files", `{"action":"save","file_path":"/files/e.js"}`, `{"success":false,"code":"undefined_reference","error":"uses a name nothing binds"}`},
		{"a2", "manage_files", `{"action":"save","file_path":"/files/e.js"}`, `{"success":false,"code":"entry_exports","error":"entry declares exports"}`},
		{"a3", "manage_files", `{"action":"save","file_path":"/files/e.js"}`, `{"success":true}`},
	})
	for _, f := range sequential.Findings {
		if f.Type == "patch_loop" {
			t.Errorf("different codes on one file must not read as a stuck loop: %+v", f)
		}
	}
	if sequential.Score != 0 {
		t.Errorf("two healthy catches fixed in sequence must not score, got %d", sequential.Score)
	}

	// The same code twice IS a loop - the rejection message did not land - even
	// when the code is a healthy pre-ship catch.
	repeated := scanMini(t, "completed", []toolStep{
		{"b1", "manage_files", `{"action":"patch","file_path":"/files/f.js"}`, `{"success":false,"code":"syntax_error","error":"would introduce a syntax error"}`},
		{"b2", "manage_files", `{"action":"patch","file_path":"/files/f.js"}`, `{"success":false,"code":"syntax_error","error":"would introduce a syntax error"}`},
	})
	var loop *triageFinding
	for i := range repeated.Findings {
		if repeated.Findings[i].Type == "patch_loop" {
			loop = &repeated.Findings[i]
		}
	}
	if loop == nil || loop.Code != "syntax_error" || loop.Count != 2 {
		t.Errorf("a same-code repeat must read as a stuck loop, got %+v", repeated.Findings)
	}
	if repeated.Score == 0 {
		t.Error("a genuine stuck loop must score")
	}
}

// A friction reject (a bad tool call, a patch miss) is real build cost and must
// still push the build to noisy.
func TestTriage_FrictionRejectStillScores(t *testing.T) {
	tb := scanMini(t, "completed", []toolStep{
		{"a1", "manage_files", `{"action":"patch","file_path":"/files/a.js"}`, `{"success":false,"code":"patch_anchor_not_found","error":"anchors were not found"}`},
	})
	if tb.Score == 0 || tb.Verdict != "noisy" {
		t.Errorf("a friction reject must score: score=%d verdict=%q", tb.Score, tb.Verdict)
	}
}

// A probe red on a target later re-probed green is the model's own verify loop -
// discounted. An unresolved red still scores.
func TestTriage_ProbeResolutionRecalibratesScore(t *testing.T) {
	resolved := scanMini(t, "completed", []toolStep{
		{"p1", "manage_testing", `{"action":"test_page","page_path":"/"}`, `{"success":false,"error":"page response too short"}`},
		{"p2", "manage_testing", `{"action":"test_page","page_path":"/"}`, `{"success":true}`},
	})
	if resolved.Score != 0 || resolved.Verdict != "clean" {
		t.Errorf("a probe red cleared by a later green probe must not score: score=%d verdict=%q", resolved.Score, resolved.Verdict)
	}
	// Cross-action: a 503 on `/` via test_url (the build-in-progress holding page),
	// then a green test_page on `/` once it is live. Same public route, so the green
	// clears the red even though the probe actions differ.
	crossAction := scanMini(t, "completed", []toolStep{
		{"p1", "manage_testing", `{"action":"test_url","url_path":"/"}`, `{"success":false,"error":"expected a 2xx status, got 503"}`},
		{"p2", "manage_testing", `{"action":"test_page","page_path":"/"}`, `{"success":true}`},
	})
	if crossAction.Score != 0 || crossAction.Verdict != "clean" {
		t.Errorf("a test_url red on a route cleared by a test_page green must not score: score=%d verdict=%q", crossAction.Score, crossAction.Verdict)
	}
	unresolved := scanMini(t, "completed", []toolStep{
		{"p1", "manage_testing", `{"action":"test_url","url_path":"/api/x"}`, `{"success":false,"error":"expected a 2xx status, got 404"}`},
	})
	if unresolved.Score == 0 || unresolved.Verdict != "noisy" {
		t.Errorf("an unresolved probe red must score: score=%d verdict=%q", unresolved.Score, unresolved.Verdict)
	}
}

// triageTestSchema is the minimal slice of the project schema the scan reads:
// runs, the brain chat dialogue, the LLM calls (whose request_messages carry the
// full code-bearing tool result, and whose prefix/cache columns carry the cost
// picture), and the run log the validator and stage streams land in.
const triageTestSchema = `
CREATE TABLE of_runs (
  id TEXT PRIMARY KEY, status TEXT, stage TEXT,
  started_at TEXT, completed_at TEXT,
  error_count INTEGER DEFAULT 0, last_error TEXT DEFAULT ''
);
CREATE TABLE of_chat_messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, role TEXT,
  content TEXT, tool_calls TEXT, tool_call_id TEXT, created_at TEXT
);
CREATE TABLE of_llm_calls (
  id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT, source TEXT,
  model TEXT, request_messages TEXT, error_message TEXT,
  stage TEXT DEFAULT '', iteration INTEGER DEFAULT 0,
  session_id TEXT DEFAULT '', provider_type TEXT DEFAULT '',
  input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
  cache_read_input_tokens INTEGER DEFAULT 0,
  cache_creation_input_tokens INTEGER DEFAULT 0,
  stable_prefix_bytes INTEGER DEFAULT 0, stable_prefix_msgs INTEGER DEFAULT 0,
  total_request_bytes INTEGER DEFAULT 0, duration_ms INTEGER DEFAULT 0,
  response_tool_calls TEXT, created_at TEXT
);
CREATE TABLE of_run_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT, kind TEXT, ref TEXT,
  payload TEXT, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
  duration_ms INTEGER DEFAULT 0, created_at TEXT
);`

func seedTriageDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(triageTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, status, stage, started_at, completed_at)
		 VALUES ('run_test_1','completed','build','2026-01-01T00:00:00Z','2026-01-01T01:00:00Z')`,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	asst := func(ts, id, name, args string) {
		tc := `[{"id":"` + id + `","name":"` + name + `","arguments":` + jsonStr(args) + `}]`
		if _, err := db.Exec(
			`INSERT INTO of_chat_messages (session_id, role, tool_calls, created_at) VALUES ('brain','assistant',?,?)`,
			tc, ts); err != nil {
			t.Fatalf("seed asst: %v", err)
		}
	}
	tool := func(ts, id, content string) {
		if _, err := db.Exec(
			`INSERT INTO of_chat_messages (session_id, role, content, tool_call_id, created_at) VALUES ('brain','tool',?,?,?)`,
			content, id, ts); err != nil {
			t.Fatalf("seed tool: %v", err)
		}
	}

	// Three failed edits to the same file (the getHooks-style loop). of_chat_messages
	// carries only the truncated {success,error} - no code.
	undefRej := `{"success":false,"error":"write rejected: x.ocean.js uses name(s) that are neither declared in this module, imported, nor a known browser/JS global"}`
	for i, id := range []string{"c1", "c2", "c3"} {
		asst("2026-01-01T00:0"+itoa(i)+":01Z", id, "manage_files", `{"action":"patch","file_path":"/files/x.ocean.js"}`)
		tool("2026-01-01T00:0"+itoa(i)+":02Z", id, undefRej)
	}
	// A task-ordering reject.
	asst("2026-01-01T00:10:01Z", "c4", "manage_tasks", `{"action":"complete"}`)
	tool("2026-01-01T00:10:02Z", "c4", `{"success":false,"error":"task 'state-leaf' depends on [game-spine], which is not complete yet."}`)
	// A runtime probe failure.
	asst("2026-01-01T00:11:01Z", "c5", "manage_testing", `{"action":"test_page","page_path":"/"}`)
	tool("2026-01-01T00:11:02Z", "c5", `{"success":false,"error":"page logged a fatal error from its own code during load"}`)
	// A successful call so total calls exceed errors.
	asst("2026-01-01T00:12:01Z", "c6", "manage_files", `{"action":"save","file_path":"/files/ok.js"}`)
	tool("2026-01-01T00:12:02Z", "c6", `{"success":true}`)

	// The code-bearing full results live in the LLM call's request_messages.
	reqMsgs := `[` +
		toolResultMsg("c1", "undefined_reference") + `,` +
		toolResultMsg("c2", "undefined_reference") + `,` +
		toolResultMsg("c3", "undefined_reference") + `]`
	if _, err := db.Exec(
		`INSERT INTO of_llm_calls (run_id, source, model, request_messages, error_message) VALUES ('run_test_1','engine','glm-test',?, '')`,
		reqMsgs); err != nil {
		t.Fatalf("seed llm call: %v", err)
	}
	return db
}

func TestTriage_ClassifiesSailingLoop(t *testing.T) {
	db := seedTriageDB(t)
	defer db.Close()

	runs := listRuns(db, "", 40)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	tb := scanRun(db, 17, "sailing", runs[0])

	if tb.ToolCalls != 6 {
		t.Errorf("tool_calls = %d, want 6", tb.ToolCalls)
	}
	if tb.ToolErrors != 5 {
		t.Errorf("tool_errors = %d, want 5", tb.ToolErrors)
	}
	if tb.Model != "glm-test" {
		t.Errorf("model = %q, want glm-test", tb.Model)
	}

	byType := map[string]triageFinding{}
	for _, f := range tb.Findings {
		byType[f.Type] = f
	}

	loop, ok := byType["patch_loop"]
	if !ok || loop.File != "/files/x.ocean.js" || loop.Count != 3 {
		t.Errorf("patch_loop finding wrong: %+v", loop)
	}
	if loop.Code != "undefined_reference" {
		t.Errorf("patch_loop code = %q, want undefined_reference (recovered from request_messages)", loop.Code)
	}
	rej, ok := byType["tool_reject"]
	if !ok || rej.Tool != "manage_files" || rej.Code != "undefined_reference" || rej.Count != 3 {
		t.Errorf("tool_reject finding wrong: %+v", rej)
	}
	if to, ok := byType["task_ordering"]; !ok || to.Count != 1 {
		t.Errorf("task_ordering finding wrong: %+v", to)
	}
	if pf, ok := byType["probe_failure"]; !ok || pf.Count != 1 {
		t.Errorf("probe_failure finding wrong: %+v", pf)
	}
	if tb.Score <= 0 {
		t.Errorf("score = %d, want > 0", tb.Score)
	}
}

// TestTriage_CleanRunNoFindings verifies an all-green run produces nothing.
func TestTriage_CleanRunNoFindings(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(triageTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db.Exec(`INSERT INTO of_runs (id, status, stage, started_at, completed_at)
	         VALUES ('r','completed','build','2026-01-01T00:00:00Z','2026-01-01T01:00:00Z')`)
	db.Exec(`INSERT INTO of_chat_messages (session_id, role, tool_calls, created_at) VALUES ('brain','assistant','[{"id":"c1","name":"manage_files","arguments":"{}"}]','2026-01-01T00:00:01Z')`)
	db.Exec(`INSERT INTO of_chat_messages (session_id, role, content, tool_call_id, created_at) VALUES ('brain','tool','{"success":true}','c1','2026-01-01T00:00:02Z')`)

	tb := scanRun(db, 1, "clean", listRuns(db, "", 40)[0])
	if len(tb.Findings) != 0 {
		t.Errorf("clean run should have no findings, got %+v", tb.Findings)
	}
	if tb.ToolErrors != 0 {
		t.Errorf("clean run tool_errors = %d, want 0", tb.ToolErrors)
	}
}

// thrashCalls is a build-stage call series carrying the cost the tool-error lens
// cannot see: {iteration, input_tokens, cache_read, stable_prefix_bytes,
// total_request_bytes}. Every call succeeds and the run completes, yet the
// history is rewritten five times and one call is served nothing from cache -
// together re-buying ~140K tokens at full price.
var thrashCalls = [][5]int{
	{0, 36713, 832, 0, 154998},
	{1, 302, 37504, 73502, 156093},
	{2, 5250, 37760, 74597, 172952},
	{3, 2913, 43008, 91456, 184308},
	{4, 149, 45888, 102812, 184830},
	{5, 49621, 0, 103334, 198909}, // cold miss: cache_read=0, 49.6K fresh
	{6, 1280, 49600, 117413, 204002},
	{7, 8709, 50816, 122506, 232768},
	{8, 312, 59520, 151272, 234058},
	{9, 5037, 59776, 152562, 248518},
	{10, 6703, 64768, 167022, 271289},
	{11, 26648, 37824, 129551, 250728}, // prefix -37471B, 26.6K re-billed
	{12, 1965, 64448, 169232, 257008},
	{13, 20485, 41856, 146102, 242859}, // prefix -23130B
	{14, 4066, 62336, 161363, 253461},
	{15, 20552, 43968, 144686, 241873}, // prefix -16677B
	{16, 2227, 64512, 160377, 248846},
	{17, 25939, 47424, 162831, 267736},
	{18, 24012, 47552, 171481, 262385},
	{19, 15407, 47744, 156148, 241063}, // prefix -15333B
	{20, 678, 63104, 159567, 243501},
	{21, 331, 63744, 162005, 244506},
	{22, 3979, 64064, 163010, 259793},
	{23, 6923, 48128, 141630, 224268}, // prefix -21380B
	{24, 141, 55040, 142772, 224809},
}

// seedThrashDB seeds an error-free run whose cost lives entirely in the
// prefix/cache series. No tool call fails, so the error lens sees nothing.
func seedThrashDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(triageTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_runs (id, status, stage, started_at, completed_at)
		VALUES ('run_thrash','completed','live','2026-01-01T00:00:00Z','2026-01-01T00:12:15Z')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// One engine segment for the whole build, so every prefix collapse below is
	// a rewrite within a live conversation rather than a segment boundary.
	for _, c := range thrashCalls {
		if _, err := db.Exec(
			`INSERT INTO of_llm_calls
			 (run_id, source, model, stage, session_id, iteration, input_tokens, output_tokens,
			  cache_read_input_tokens, stable_prefix_bytes, total_request_bytes, error_message)
			 VALUES ('run_thrash','engine','glm-test','build','sess_build',?,?,0,?,?,?,'')`,
			c[0], c[1], c[2], c[3], c[4]); err != nil {
			t.Fatalf("seed llm call: %v", err)
		}
	}
	return db
}

// shredCall builds one build turn: tiny (40 output tokens) unless big, with its calls.
func shredCall(id int64, sess string, big bool, calls ...toolRef) runCall {
	c := runCall{ID: id, Stage: "build", SessionID: sess, CacheRead: 100_000, Output: 40, ToolCalls: len(calls), Calls: calls}
	if big {
		c.Output = 2000
	}
	return c
}

func save(id string) toolRef     { return toolRef{ID: id, Name: "manage_files", Action: "save"} }
func testPage(id string) toolRef { return toolRef{ID: id, Name: "manage_testing", Action: "test_page"} }
func complete(id string) toolRef { return toolRef{ID: id, Name: "manage_tasks", Action: "complete"} }
func get(id string) toolRef      { return toolRef{ID: id, Name: "manage_files", Action: "get"} }

// Only AVOIDABLE tiny turns count: a lone test, complete or small save right after a
// turn of successful saves could have ridden that turn (calls in a turn run in order).
// A tiny turn that had to wait on a result - the session's first, one after a failed
// call, one after a platform message, one acting on what it just read or tested - is
// reported as needed, never charged.
func TestDetectShredding(t *testing.T) {
	failedIDs := map[string]bool{"s5": true}
	failed := func(id string) bool { return failedIDs[id] }
	calls := []runCall{
		shredCall(1, "a", false, save("s1")),                                     // session start: needed
		shredCall(2, "a", false, testPage("t2")),                                 // after a successful save: avoidable
		shredCall(3, "a", false, complete("c3")),                                 // after a test it may act on: needed
		shredCall(4, "a", true, save("s4")),                                      // big turn: not tiny
		shredCall(5, "a", false, save("s5"), complete("c5")),                     // after saves: avoidable (s5 fails)
		shredCall(6, "a", false, save("s6")),                                     // after a failed call: needed
		shredCall(7, "a", false, complete("c7")),                                 // after a successful save: avoidable
		shredCall(8, "a", false, get("g8")),                                      // reads: needed
		shredCall(9, "a", true, save("s9")),                                      // big
		shredCall(10, "a", false, testPage("t10")),                               // injected nudge before it: needed
		shredCall(11, "a", false, testPage("t11"), complete("c11")),              // after a test: needed
		shredCall(12, "b", false, save("s12")),                                   // new session: needed
		shredCall(13, "b", false, testPage("t13")),                               // after a save: avoidable
		{ID: 14, Stage: "build", SessionID: "b", Output: 20, CacheRead: 100_000}, // closing narration
	}
	sh := detectShredding(calls, failed, map[int64]bool{10: true})
	if sh.Turns != 4 || sh.Needed != 7 || sh.StageTurns != 14 {
		t.Fatalf("shredding = %+v, want 4 avoidable, 7 needed of 14", sh)
	}
	if sh.Tokens != 400_000 {
		t.Errorf("tokens = %d, want 400000 (what the 4 counted turns billed)", sh.Tokens)
	}
}

// A couple of avoidable turns is noise, not a pattern; PLAN is single-shot by design.
func TestDetectShreddingNeedsAPatternAndIgnoresOtherStages(t *testing.T) {
	calls := []runCall{
		shredCall(1, "a", true, save("s1")),
		shredCall(2, "a", false, testPage("t2")),
		shredCall(3, "a", true, save("s3")),
		shredCall(4, "a", false, testPage("t4")),
	}
	if sh := detectShredding(calls, nil, nil); sh.Turns != 0 || sh.Tokens != 0 {
		t.Errorf("2 avoidable turns is below the floor, got %+v", sh)
	}
	plan := []runCall{{Stage: "plan", ToolCalls: 1, Output: 20, CacheRead: 100_000}}
	if sh := detectShredding(plan, nil, nil); sh.Turns != 0 || sh.StageTurns != 0 {
		t.Errorf("PLAN is single-shot by design, got %+v", sh)
	}
}

// The call walk names each turn's calls in SQL (arguments never cross into Go), and
// requestFacts flags a request the platform injected a message into - but not one
// that only carries the model's own tool results and the snapshot delta.
func TestShreddingFactsFromTheDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(triageTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	ins := func(tools, msgs string) {
		if _, err := db.Exec(`INSERT INTO of_llm_calls (run_id, source, model, stage, response_tool_calls, request_messages)
			VALUES ('r1','engine','m','build',?,?)`, tools, msgs); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`[{"id":"a1","name":"manage_files","arguments":"{\"action\":\"save\",\"body\":\"x\"}"},{"id":"a2","name":"manage_tasks","arguments":"not json"}]`,
		`[{"role":"user","content":"build it"}]`)
	ins(`[{"id":"b1","name":"manage_testing","arguments":{"action":"test_page"}}]`,
		`[{"role":"assistant","content":""},{"role":"tool","tool_call_id":"a1","content":"{\"success\":true}"},{"role":"user","content":"<!--__of_snapshot:delta__--> x"}]`)
	ins(`[]`, `{"base":2,"prefix":1,"msgs":[{"role":"assistant","content":""},{"role":"user","content":"[mode: fix-pass] (round 1)"}]}`)

	calls := loadRunCalls(db, "r1")
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(calls))
	}
	if got := calls[0].Calls; len(got) != 2 || got[0] != (toolRef{"a1", "manage_files", "save"}) || got[1] != (toolRef{"a2", "manage_tasks", ""}) {
		t.Errorf("first turn's calls = %+v", got)
	}
	if got := calls[1].Calls; len(got) != 1 || got[0].Action != "test_page" {
		t.Errorf("an object-valued arguments must still yield its action, got %+v", got)
	}
	_, injected := requestFacts(db, "r1")
	if injected[calls[1].ID] {
		t.Error("tool results plus a snapshot delta are not a platform message")
	}
	if !injected[calls[2].ID] {
		t.Error("a fix-pass instruction is a platform message")
	}
}

// Work the plan owed that never shipped. Validators only inspect what was built, so
// without this a build that dropped a checklist item reads exactly like a clean one.
func TestUnbuiltFindingsScoreTheUndeliveredPlan(t *testing.T) {
	f, score := unbuiltFindings([]string{"Projects gallery at /work"})
	if len(f) != 1 || f[0].Type != "unbuilt_task" || f[0].Count != 1 {
		t.Fatalf("findings = %+v, want one unbuilt_task", f)
	}
	if !strings.Contains(f[0].Detail, "Projects gallery at /work") {
		t.Errorf("detail must name the item, got %q", f[0].Detail)
	}
	if score != unbuiltTaskWeight {
		t.Errorf("score = %d, want %d", score, unbuiltTaskWeight)
	}
	if f, score := unbuiltFindings(nil); f != nil || score != 0 {
		t.Errorf("a finished checklist is not a finding, got %+v / %d", f, score)
	}
}

// TestTriage_CacheThrashRunScoresHigh pins that a run with zero failed tool
// calls, five history rewrites re-billing ~90K tokens, and a cold miss re-billing
// another ~50K, does not read as clean.
func TestTriage_CacheThrashRunScoresHigh(t *testing.T) {
	db := seedThrashDB(t)
	defer db.Close()

	tb := scanRun(db, 1, "thrash", listRuns(db, "", 40)[0])

	if tb.ToolErrors != 0 {
		t.Fatalf("fixture must be error-free, got tool_errors = %d", tb.ToolErrors)
	}

	byType := map[string]triageFinding{}
	for _, f := range tb.Findings {
		byType[f.Type] = f
	}
	if th, ok := byType["cache_thrash"]; !ok || th.Count != 5 {
		t.Errorf("cache_thrash finding = %+v, want count 5", th)
	}
	if cm, ok := byType["cold_miss"]; !ok || cm.Count != 1 {
		t.Errorf("cold_miss finding = %+v, want count 1", cm)
	}
	if tb.Score < 10 {
		t.Errorf("score = %d, want >= 10: an error-free run that re-billed ~140K tokens is not clean", tb.Score)
	}
}

// TestTriage_FileChurn separates work the run redid from ordinary discovery: a
// read of a file this run never wrote is the model learning the codebase, not
// paging its own output back in.
func TestTriage_FileChurn(t *testing.T) {
	dialogue := []dialogueMsg{{toolCalls: `[
		{"id":"1","name":"manage_files","arguments":"{\"action\":\"get\",\"file_path\":\"/files/vendor.js\"}"},
		{"id":"2","name":"manage_files","arguments":"{\"action\":\"save\",\"file_path\":\"/files/game.js\"}"},
		{"id":"3","name":"manage_files","arguments":"{\"action\":\"save\",\"file_path\":\"/files/game.js\"}"},
		{"id":"4","name":"manage_files","arguments":"{\"action\":\"get\",\"file_path\":\"/files/game.js\"}"},
		{"id":"5","name":"manage_files","arguments":"{\"action\":\"get\",\"file_path\":\"/files/game.js\"}"},
		{"id":"6","name":"manage_files","arguments":"{\"action\":\"save\",\"file_path\":\"/files/once.js\"}"}
	]`}}

	churn := detectFileChurn(dialogue)
	if churn.saves["/files/game.js"] != 2 {
		t.Errorf("saves[game.js] = %d, want 2", churn.saves["/files/game.js"])
	}
	if churn.rereads["/files/game.js"] != 2 {
		t.Errorf("rereads[game.js] = %d, want 2", churn.rereads["/files/game.js"])
	}
	if n, ok := churn.rereads["/files/vendor.js"]; ok {
		t.Errorf("rereads[vendor.js] = %d, want absent: the run never wrote it", n)
	}
	if got := repeatedTargets(churn.saves, 2); len(got) != 1 || got[0] != "/files/game.js" {
		t.Errorf("redundant saves = %v, want [/files/game.js]", got)
	}
}

// TestTriage_RejectedWriteIsNotChurn pins the boundary between the two lenses. A
// write the gate rejected is the error lens's finding; the corrected re-save that
// follows is the run recovering, not redoing work. Counting it as churn bills one
// event to both lenses and inflates the score.
func TestTriage_RejectedWriteIsNotChurn(t *testing.T) {
	rejected := `{"success":false,"error":"save rejected: would have a syntax error","code":"syntax_error"}`
	ok := `{"success":true}`

	dialogue := []dialogueMsg{{
		toolCalls: `[
			{"id":"a","name":"manage_files","arguments":"{\"action\":\"save\",\"file_path\":\"/files/a.js\"}"},
			{"id":"b","name":"manage_files","arguments":"{\"action\":\"save\",\"file_path\":\"/files/a.js\"}"},
			{"id":"c","name":"manage_files","arguments":"{\"action\":\"get\",\"file_path\":\"/files/a.js\"}"},
			{"id":"d","name":"manage_files","arguments":"{\"action\":\"save\",\"file_path\":\"/files/b.js\"}"},
			{"id":"e","name":"manage_files","arguments":"{\"action\":\"save\",\"file_path\":\"/files/b.js\"}"}
		]`,
		resultByID: map[string]string{
			"a": rejected, // rejected, so a.js is written exactly once
			"b": ok,
			"c": ok,
			"d": ok, // b.js is genuinely written twice
			"e": ok,
		},
	}}

	churn := detectFileChurn(dialogue)
	if got := churn.saves["/files/a.js"]; got != 1 {
		t.Errorf("saves[a.js] = %d, want 1: a rejected save wrote nothing", got)
	}
	if got := churn.saves["/files/b.js"]; got != 2 {
		t.Errorf("saves[b.js] = %d, want 2", got)
	}
	if got := repeatedTargets(churn.saves, 2); len(got) != 1 || got[0] != "/files/b.js" {
		t.Errorf("redundant saves = %v, want only [/files/b.js]", got)
	}
}

// TestTriage_RunLogStreams covers the of_run_log signals the tool-error lens
// cannot see: the validators' verdict on the shipped artifacts, and tasks landing
// in one batch.
func TestTriage_RunLogStreams(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(triageTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	logRow := func(kind, ref, payload string) {
		if _, err := db.Exec(
			`INSERT INTO of_run_log (run_id, kind, ref, payload) VALUES ('r',?,?,?)`,
			kind, ref, payload); err != nil {
			t.Fatalf("seed run_log: %v", err)
		}
	}
	logRow("issue", "authoritative_reducer",
		`{"issues":[{"severity":"error","category":"websocket","page":"/api/room/ws","message":"broken reducer: threw on @join"}]}`)
	logRow("issue", "static_code",
		`{"issues":[{"severity":"warning","category":"undefined_reference","file":"app.widget.js","message":"uses a name no scope binds"}]}`)
	logRow("stage", "build", `{"status":"failed","error":"stage gave up"}`)
	logRow("stage", "plan", `{"status":"completed"}`)
	for i := 0; i < 4; i++ {
		logRow("task", "t"+itoa(i), `{"iteration":11,"title":"leaf"}`)
	}
	logRow("task", "early", `{"iteration":2,"title":"shell"}`)

	rl := loadRunLog(db, "r")

	if got := countValidatorErrors(rl.Issues); got != 1 {
		t.Errorf("validator errors = %d, want 1", got)
	}
	valF, valScore := validatorFindings(rl.Issues)
	if len(valF) != 2 {
		t.Fatalf("validator findings = %d, want 2 (one per validator)", len(valF))
	}
	if want := validatorErrorWeight + validatorWarningWeight; valScore != want {
		t.Errorf("validator score = %d, want %d", valScore, want)
	}

	if len(rl.StageFailures) != 1 || rl.StageFailures[0].Stage != "build" {
		t.Errorf("stage failures = %+v, want just build (completed stages are not failures)", rl.StageFailures)
	}

	n, iter := rl.batchedTasks()
	if n != 4 || iter != 11 {
		t.Errorf("batchedTasks = (%d, it%d), want (4, it11)", n, iter)
	}
}

// The engine records the FINAL validator sweep once, and it is the verdict on what
// shipped. A fix-pass that resolves a round-1 issue makes it stop appearing, so
// triage must count the final sweep, not the per-round history - otherwise a fixed
// build reads as broken.
func TestTriage_FinalValidationReconciles(t *testing.T) {
	newDB := func() *sql.DB {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := db.Exec(triageTestSchema); err != nil {
			t.Fatalf("schema: %v", err)
		}
		return db
	}
	logRow := func(db *sql.DB, kind, ref, payload string) {
		if _, err := db.Exec(
			`INSERT INTO of_run_log (run_id, kind, ref, payload) VALUES ('r',?,?,?)`,
			kind, ref, payload); err != nil {
			t.Fatalf("seed run_log: %v", err)
		}
	}

	// Round 1 flagged auth_gate; the fix-pass cleared it, so the final sweep is empty.
	t.Run("fixed issue is not counted", func(t *testing.T) {
		db := newDB()
		defer db.Close()
		logRow(db, "issue", "auth_gate",
			`{"issues":[{"severity":"error","category":"auth_gate","file":"events","message":"requires a logged-in user but there is no login surface"}]}`)
		logRow(db, "stage", "build", `{"status":"completed"}`)
		logRow(db, "validation", "", `{"issues":null}`)

		rl := loadRunLog(db, "r")
		if got := countValidatorErrors(rl.Issues); got != 0 {
			t.Errorf("validator errors = %d, want 0 (the fix-pass cleared it; the final sweep is authoritative)", got)
		}
		if valF, _ := validatorFindings(rl.Issues); len(valF) != 0 {
			t.Errorf("validator findings = %d, want 0", len(valF))
		}
	})

	// An error that survives every fix-pass is in the final sweep and still ships.
	t.Run("surviving issue is counted, keyed by category", func(t *testing.T) {
		db := newDB()
		defer db.Close()
		logRow(db, "issue", "runtime_probe",
			`{"issues":[{"severity":"error","category":"runtime","page":"/","message":"threw on load"}]}`)
		logRow(db, "stage", "build", `{"status":"completed"}`)
		logRow(db, "validation", "",
			`{"issues":[{"severity":"error","category":"runtime","page":"/","message":"threw on load"}]}`)

		rl := loadRunLog(db, "r")
		if got := countValidatorErrors(rl.Issues); got != 1 {
			t.Errorf("validator errors = %d, want 1 (it survived the fix loop and shipped)", got)
		}
		if len(rl.Issues["runtime"]) != 1 {
			t.Errorf("surviving issue should be keyed by category 'runtime', got %v", rl.Issues)
		}
	})
}

// TestTriage_SegmentBoundaryIsNotThrash pins that a new engine segment is not a
// history rewrite. Each segment opens with a zero prefix and buys its context
// once; measuring that against the previous segment's last call reports a
// collapse of the entire prior conversation that never happened.
func TestTriage_SegmentBoundaryIsNotThrash(t *testing.T) {
	calls := []runCall{
		{Iteration: 0, Stage: "build", SessionID: "s1", Input: 39710, CacheRead: 1280, PrefixBytes: 0},
		{Iteration: 1, Stage: "build", SessionID: "s1", Input: 4990, CacheRead: 40960, PrefixBytes: 80568},
		{Iteration: 2, Stage: "build", SessionID: "s1", Input: 2708, CacheRead: 45888, PrefixBytes: 99619},
		// New segment: prefix resets to 0 and the context is re-bought legitimately.
		{Iteration: 0, Stage: "live", SessionID: "s2", Input: 17166, CacheRead: 832, PrefixBytes: 0},
		{Iteration: 1, Stage: "live", SessionID: "s2", Input: 10340, CacheRead: 8000, PrefixBytes: 7565},
	}
	_, events := detectCost(calls)
	for _, e := range events {
		t.Errorf("segment boundary flagged as %s at it%d (drop %dB): a new segment is not a rewrite",
			e.Kind, e.Iteration, e.PrefixDrop)
	}
}

// TestTriage_BootFailureNeverReachesClassify pins the contract that makes a
// data.boot_failures branch in classifyResult unreachable: a probe that logs boot
// failures hard-fails, and every summarizer routes a failed result through
// tools.SummarizeError, which emits {success,error,code} and drops data. What the
// scan can see is the tool name and the error text - never the payload.
func TestTriage_BootFailureNeverReachesClassify(t *testing.T) {
	full := `{"success":false,"error":"page logged a fatal error from its own code during load",` +
		`"data":{"boot_failures":[{"page":"/","message":"ReferenceError: THREE is not defined"}]}}`

	for _, summarize := range []struct {
		name string
		fn   func(string) string
	}{
		{"SummarizeStripBodies", tools.SummarizeStripBodies},
		{"GenericSummarize", tools.GenericSummarize},
	} {
		t.Run(summarize.name, func(t *testing.T) {
			stored := summarize.fn(full)
			if strings.Contains(stored, "boot_failures") {
				t.Fatalf("premise broken: %s kept data, got %s", summarize.name, stored)
			}
			failed, code, errText := classifyResult(stored)
			if !failed {
				t.Errorf("failed = false, want true")
			}
			if code == "boot_failure" {
				t.Errorf("code = boot_failure, but no summarizer can produce it")
			}
			if errText == "" {
				t.Errorf("error text must survive: it is all the scan gets")
			}
		})
	}
}

// --- tiny local helpers (no strconv/encoding import noise in a test file) ---

func itoa(n int) string { return string(rune('0' + n)) }

// jsonStr wraps a raw arguments string as a JSON string literal (the shape the
// tool_calls column stores: arguments is itself a JSON-encoded string).
func jsonStr(s string) string {
	out := []rune{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		default:
			out = append(out, r)
		}
	}
	out = append(out, '"')
	return string(out)
}

// toolResultMsg builds one request_messages entry: a role=tool message whose
// content is a full result carrying the given code.
func toolResultMsg(id, code string) string {
	content := `{"success":false,"error":"write rejected: neither declared in this module","code":"` + code + `"}`
	return `{"role":"tool","tool_call_id":"` + id + `","content":` + jsonStr(content) + `}`
}

// TestCodesByToolCallID_SurvivesCompactionRewrites: codes come from each tool
// result's FIRST appearance across the whole run, so a later row whose replayed
// history carries a compaction-summarized body (code stripped) cannot mask the
// original, and a delta row contributes exactly what it appended.
func TestCodesByToolCallID_SurvivesCompactionRewrites(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(triageTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	summarized := func(id string) string {
		content := `{"success":false,"error":"missing required argument"}`
		return `{"role":"tool","tool_call_id":"` + id + `","content":` + jsonStr(content) + `}`
	}
	insert := func(msgs string) {
		if _, err := db.Exec(
			`INSERT INTO of_llm_calls (run_id, source, model, request_messages) VALUES ('run_cc','engine','m',?)`,
			msgs); err != nil {
			t.Fatalf("seed call: %v", err)
		}
	}
	// Turn 1: the original full results, codes intact.
	insert(`[` + toolResultMsg("c1", "missing_arg") + `,` + toolResultMsg("c2", "missing_arg") + `]`)
	// Turn 2: a post-compaction replay - the same ids now carry summarized,
	// code-less bodies, plus one genuinely new result.
	insert(`[` + summarized("c1") + `,` + summarized("c2") + `,` + toolResultMsg("c3", "patch_anchor_not_found") + `]`)
	// Turn 3: a delta row; only its appended message counts.
	insert(`{"base":2,"prefix":3,"msgs":[` + toolResultMsg("c4", "syntax_error") + `]}`)

	codes := codesByToolCallID(db, "run_cc")
	want := map[string]string{
		"c1": "missing_arg",
		"c2": "missing_arg",
		"c3": "patch_anchor_not_found",
		"c4": "syntax_error",
	}
	for id, code := range want {
		if codes[id] != code {
			t.Errorf("codes[%q] = %q, want %q", id, codes[id], code)
		}
	}
}

func findingOf(tb triageBuild, typ string) *triageFinding {
	for i := range tb.Findings {
		if tb.Findings[i].Type == typ {
			return &tb.Findings[i]
		}
	}
	return nil
}

// A red user-flow replay that goes green is the flow tests catching a bug before
// ship: surfaced as flow_catch, never as probe_failure, and not scored. A replay
// still red at the end is flow_failure and scores. A red clears only when the flows
// it named pass, so a green of one other flow does not clear it.
func TestTriage_FlowReplaysAreTheirOwnFinding(t *testing.T) {
	red := `{"success":false,"error":"2 of 5 flows failed: a, b"}`
	green := `{"success":true,"data":{"failed":null,"passed":["a","b","c","d","e"]}}`
	caught := scanMini(t, "completed", []toolStep{
		{"w1", "manage_testing", `{"action":"write_flows","source":"x"}`, `{"success":true}`},
		{"f1", "manage_testing", `{"action":"test_flows"}`, red},
		{"f2", "manage_testing", `{"action":"test_flows"}`, red},
		{"f3", "manage_testing", `{"action":"test_flows"}`, green},
	})
	if caught.Score != 0 || caught.Verdict != "clean" {
		t.Errorf("flow reds cleared by a green replay must not score: score=%d verdict=%q", caught.Score, caught.Verdict)
	}
	if f := findingOf(caught, "flow_catch"); f == nil || f.Count != 2 {
		t.Errorf("want flow_catch x2, got %+v", caught.Findings)
	}
	if findingOf(caught, "probe_failure") != nil {
		t.Errorf("a flow replay must not be reported as a probe failure: %+v", caught.Findings)
	}

	stillRed := scanMini(t, "completed", []toolStep{
		{"f1", "manage_testing", `{"action":"test_flows"}`, red},
		{"f2", "manage_testing", `{"action":"test_flows","flow":"a"}`, `{"success":true,"data":{"passed":["a"]}}`},
	})
	if f := findingOf(stillRed, "flow_failure"); f == nil || f.Count != 1 {
		t.Errorf("want flow_failure x1 (b never passed), got %+v", stillRed.Findings)
	}
	if stillRed.Score == 0 || stillRed.Verdict != "noisy" {
		t.Errorf("an unresolved flow red must score: score=%d verdict=%q", stillRed.Score, stillRed.Verdict)
	}
}

// What does not clear a flow red: a replay refused by the budget or skipped
// (success, but nothing ran), and a full green of a suite rewritten to other flows.
// Named greens that together cover the red's flows do clear it, even when a flow
// name itself contains ", ".
func TestTriage_FlowRedsClearOnlyByTheirOwnFlows(t *testing.T) {
	red := `{"success":false,"error":"2 of 3 flows failed: join, two people meet, chat, end (1 more blocked by them)"}`
	for name, clear := range map[string]string{
		"budget":  `{"_compacted":"dropped 1 bulky field(s)","data":{"budget_exhausted":true},"success":true}`,
		"skipped": `{"success":true,"data":{"skipped":"no chrome"}}`,
		"debug":   `{"success":true,"data":{"failed":null,"passed":["debug chat open"]}}`,
	} {
		tb := scanMini(t, "completed", []toolStep{
			{"f1", "manage_testing", `{"action":"test_flows"}`, red},
			{"f2", "manage_testing", `{"action":"test_flows"}`, clear},
		})
		if f := findingOf(tb, "flow_failure"); f == nil || f.Count != 1 {
			t.Errorf("%s: want flow_failure x1, got %+v", name, tb.Findings)
		}
		if findingOf(tb, "flow_catch") != nil {
			t.Errorf("%s: nothing was replayed green, so nothing was caught: %+v", name, tb.Findings)
		}
	}

	covered := scanMini(t, "completed", []toolStep{
		{"f1", "manage_testing", `{"action":"test_flows"}`, red},
		{"f2", "manage_testing", `{"action":"test_flows","flow":"join"}`, `{"success":true,"data":{"passed":["join"]}}`},
		{"f3", "manage_testing", `{"action":"test_flows","flow":"two people meet, chat, end"}`,
			`{"success":true,"data":{"passed":["two people meet, chat, end"]}}`},
	})
	if f := findingOf(covered, "flow_catch"); f == nil || f.Count != 1 || findingOf(covered, "flow_failure") != nil {
		t.Errorf("want flow_catch x1 once both named flows passed, got %+v", covered.Findings)
	}

	// A red whose error names no flows clears on any full green, as before.
	unnamed := scanMini(t, "completed", []toolStep{
		{"f1", "manage_testing", `{"action":"test_flows"}`, `{"success":false,"error":"flow run failed"}`},
		{"f2", "manage_testing", `{"action":"test_flows"}`, `{"success":true,"data":{"passed":["x"]}}`},
	})
	if f := findingOf(unnamed, "flow_catch"); f == nil || f.Count != 1 {
		t.Errorf("want flow_catch x1, got %+v", unnamed.Findings)
	}
}

// The build's own replays never went green, but the final validator sweep replayed
// of_test.js with no flow issue: the post-build fix-pass caught it before ship. A
// final sweep still carrying a flow issue leaves the red a flow_failure.
func TestTriage_FlowRedsClearedByTheFinalSweep(t *testing.T) {
	steps := []toolStep{
		{"f1", "manage_testing", `{"action":"test_flows"}`, `{"success":false,"error":"1 of 2 flows failed: chat"}`},
		{"f2", "manage_testing", `{"action":"test_flows"}`, `{"success":true,"data":{"budget_exhausted":true}}`},
	}
	withFinal := func(payload string) triageBuild {
		db := seedMini(t, "completed", steps)
		if _, err := db.Exec(
			`INSERT INTO of_run_log (run_id, kind, ref, payload) VALUES ('r','validation','',?)`, payload); err != nil {
			t.Fatalf("seed run log: %v", err)
		}
		return scanSeeded(t, db)
	}

	green := withFinal(`{"issues":null}`)
	if f := findingOf(green, "flow_catch"); f == nil || f.Count != 1 || !strings.Contains(f.Detail, "post-build replay") {
		t.Errorf("want flow_catch x1 cleared by the post-build replay, got %+v", green.Findings)
	}
	if findingOf(green, "flow_failure") != nil || green.Score != 0 {
		t.Errorf("a red the final sweep replayed green must not score: score=%d %+v", green.Score, green.Findings)
	}

	red := withFinal(`{"issues":[{"severity":"error","category":"flow","file":"of_test.js","message":"user flow \"chat\" failed"}]}`)
	if f := findingOf(red, "flow_failure"); f == nil || f.Count != 1 {
		t.Errorf("want flow_failure x1 when the final sweep is still red, got %+v", red.Findings)
	}
}

// write_flows replays every save at once, so its result is read as a replay: a red
// save cleared by a later green save is a flow_catch, while a parse reject of the
// script stays an ordinary tool reject and never counts as a flow red.
func TestTriage_WriteFlowsReplayIsAReplay(t *testing.T) {
	red := `{"success":false,"error":"1 of 2 flows failed: join"}`
	green := `{"success":true,"data":{"failed":null,"passed":["join","chat"],"saved":"of_test.js"}}`
	tb := scanMini(t, "completed", []toolStep{
		{"w0", "manage_testing", `{"action":"write_flows","source":"x"}`, `{"success":false,"error":"flow \"join\" step 2 {\"press\":\"Hyper\"}: press takes one character"}`},
		{"w1", "manage_testing", `{"action":"write_flows","source":"x"}`, red},
		{"w2", "manage_testing", `{"action":"write_flows","source":"x"}`, green},
	})
	if f := findingOf(tb, "flow_catch"); f == nil || f.Count != 1 {
		t.Errorf("a red save cleared by a green save is flow_catch x1, got %+v", tb.Findings)
	}
	if findingOf(tb, "flow_failure") != nil || findingOf(tb, "probe_failure") != nil {
		t.Errorf("a cleared write_flows red is neither a flow_failure nor a probe failure: %+v", tb.Findings)
	}
	if f := findingOf(tb, "tool_reject"); f == nil {
		t.Errorf("the parse reject must still read as a tool reject: %+v", tb.Findings)
	}
}

// TestTriage_SmallAndRecoveryResavesAreNotChurn pins the two redundant_save
// exemptions: re-saving a body small enough that a patch would cost about the same,
// and re-saving an existing file after its previous write was rejected (the
// corrected body the reject asked for).
func TestTriage_SmallAndRecoveryResavesAreNotChurn(t *testing.T) {
	big := strings.Repeat("x", redundantSaveMinBytes)
	save := func(id, file, body string) string {
		args, _ := json.Marshal(map[string]string{"action": "save", "file_path": file, "body": body})
		call, _ := json.Marshal(map[string]string{"id": id, "name": "manage_files", "arguments": string(args)})
		return string(call)
	}
	rejected := `{"success":false,"error":"write rejected","code":"undefined_reference"}`
	ok := `{"success":true}`

	dialogue := []dialogueMsg{{
		toolCalls: "[" + strings.Join([]string{
			save("1", "/files/tiny.js", "a"),
			save("2", "/files/tiny.js", "b"),
			save("3", "/files/entry.js", big),
			save("4", "/files/entry.js", big+"y"),
			save("5", "/files/entry.js", big+"z"),
			save("6", "/files/twice.js", big),
			save("7", "/files/twice.js", big+"w"),
		}, ",") + "]",
		resultByID: map[string]string{"1": ok, "2": ok, "3": ok, "4": rejected, "5": ok, "6": ok, "7": ok},
	}}

	churn := detectFileChurn(dialogue)
	if got := churn.saves["/files/tiny.js"]; got != 0 {
		t.Errorf("saves[tiny.js] = %d, want 0: small bodies are not rewrites worth a patch", got)
	}
	if got := churn.saves["/files/entry.js"]; got != 1 {
		t.Errorf("saves[entry.js] = %d, want 1: the re-save after a rejected write is recovery", got)
	}
	if got := repeatedTargets(churn.saves, 2); len(got) != 1 || got[0] != "/files/twice.js" {
		t.Errorf("redundant saves = %v, want only [/files/twice.js]", got)
	}
}
