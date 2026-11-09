package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/providers/llm"
)

// agentSeed configures the of_agent_endpoints row agentFixture writes.
type agentSeed struct {
	SystemPrompt   string
	RequiresAuth   bool
	DailyBudget    int
	DailyUsed      int
	Functions      string // JSON array literal stored on the row
	AllowDataQuery bool
	AllowHTTPFetch bool
	MaxSubagents   int
	MaxDepth       int
}

// agentFixture stands up a project with one agent endpoint at path "assistant"
// wired to a scripted provider. Mirrors llmProxyFixture (llm_endpoint_test.go),
// reusing recordingProvider from that file.
func agentFixture(t *testing.T, prov llm.Provider, seed agentSeed) (*Handler, *models.Project, *db.ProjectDB) {
	t.Helper()

	globalDB, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { globalDB.Close() })

	if _, err := globalDB.ExecWrite(`INSERT INTO providers (id, name, vendor, transport, base_url, requires_api_key)
		VALUES (1, 'Rec', 'rec', 'openai-chat', 'http://127.0.0.1:0/v1/chat/completions', 0)`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if _, err := globalDB.ExecWrite(`INSERT INTO llm_models (id, provider_id, model_id, display_name) VALUES (1, 1, 'test-model', 'Test')`); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	if _, err := globalDB.ExecWrite(`INSERT INTO projects (id, name, domain, published, mode, llm_model_id)
		VALUES (7, 'P', 'p.test', 1, 'live', 1)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(7)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}

	// day_window_start = today so the budget check does not reset the seeded
	// counter out from under a budget-exhaustion test.
	today := time.Now().UTC().Format("2006-01-02")
	maxDepth := seed.MaxDepth
	if maxDepth == 0 {
		maxDepth = 1
	}
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_agent_endpoints (path, system_prompt, max_tokens, max_steps, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, requires_auth, functions, allow_data_query, max_subagents, max_depth, allow_http_fetch)
		 VALUES ('assistant', ?, 4096, 6, 100, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seed.SystemPrompt, seed.DailyBudget, seed.DailyUsed, today, seed.RequiresAuth, seed.Functions, seed.AllowDataQuery, seed.MaxSubagents, maxDepth, seed.AllowHTTPFetch,
	); err != nil {
		t.Fatalf("seed agent endpoint: %v", err)
	}

	h := &Handler{deps: &Deps{DB: globalDB, ProjectDBManager: mgr, LLMRegistry: llm.NewRegistry()}}
	h.resolveModel = func(int) (llm.Provider, string, error) { return prov, "test-model", nil }
	project, err := models.GetProjectByID(globalDB.DB, 7)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	return h, project, pdb
}

func postRun(t *testing.T, h *Handler, project *models.Project, pdb *db.ProjectDB, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/assistant/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.apiAgent(rec, req, project, pdb, "assistant")
	return rec
}

type agentRunReply struct {
	Content    string `json:"content"`
	Steps      int    `json:"steps"`
	StopReason string `json:"stop_reason"`
}

// TestAgentRunAnswersAndBills covers the no-tool happy path: one model call, a
// direct answer, usage billed to the endpoint's daily counter, and the call
// logged under source="agent".
func TestAgentRunAnswersAndBills(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{
		{Content: "the answer", StopReason: "end", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
	h, project, pdb := agentFixture(t, prov, agentSeed{SystemPrompt: "be useful"})

	rec := postRun(t, h, project, pdb, `{"messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got agentRunReply
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if got.Content != "the answer" || got.Steps != 1 || got.StopReason != "done" {
		t.Errorf("reply = %+v, want content/steps/stop = \"the answer\"/1/done", got)
	}

	var used int
	if err := pdb.Reader().QueryRow(`SELECT daily_tokens_used FROM of_agent_endpoints WHERE path='assistant'`).Scan(&used); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if used != 15 {
		t.Errorf("daily_tokens_used = %d, want 15", used)
	}

	var source, session string
	if err := pdb.Reader().QueryRow(`SELECT source, session_id FROM of_llm_calls ORDER BY id DESC LIMIT 1`).Scan(&source, &session); err != nil {
		t.Fatalf("read log row: %v", err)
	}
	if source != "agent" || session != "assistant" {
		t.Errorf("log source/session = %q/%q, want agent/assistant", source, session)
	}
}

// TestAgentRunUsesDataQueryTool drives the real tool loop: the model calls
// query_data, the guarded read runs against an app table, and its rows are fed
// back before the final answer. Both calls are billed.
func TestAgentRunUsesDataQueryTool(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "query_data", Arguments: `{"sql":"SELECT COUNT(*) AS n FROM items","params":[]}`}}, Usage: llm.Usage{InputTokens: 8, OutputTokens: 4}},
		{Content: "there is 1 item", StopReason: "end", Usage: llm.Usage{InputTokens: 6, OutputTokens: 3}},
	}}
	h, project, pdb := agentFixture(t, prov, agentSeed{AllowDataQuery: true})

	if _, err := pdb.Writer().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := pdb.Writer().Exec(`INSERT INTO items (name) VALUES ('a')`); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	rec := postRun(t, h, project, pdb, `{"messages":[{"role":"user","content":"how many items?"}]}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got agentRunReply
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Content != "there is 1 item" || got.Steps != 2 {
		t.Errorf("reply = %+v, want content/steps = \"there is 1 item\"/2", got)
	}

	// The first request must offer the data-query tool.
	if len(prov.got) < 1 || !toolOffered(prov.got[0].Tools, "query_data") {
		t.Errorf("first request tools = %+v, want query_data offered", prov.got[0].Tools)
	}
	// The second request must carry the query rows back as a tool result.
	last := prov.got[1].Messages[len(prov.got[1].Messages)-1]
	if last.Role != llm.RoleTool || !strings.Contains(last.Content, `"n":1`) {
		t.Errorf("tool result fed back = %+v, want the query rows with n=1", last)
	}

	var used int
	if err := pdb.Reader().QueryRow(`SELECT daily_tokens_used FROM of_agent_endpoints WHERE path='assistant'`).Scan(&used); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if used != 21 {
		t.Errorf("daily_tokens_used = %d, want 21 (both calls billed)", used)
	}
}

// TestAgentTableNames verifies the table list handed to the agent includes the
// app's own tables and excludes the platform's of_* and sqlite_* ones. It guards
// the LIKE ... ESCAPE clause: an invalid escape string makes the query error and
// the list come back empty, which silently strips every table from the tool's
// description without crashing.
func TestAgentTableNames(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{Content: "x"}}}
	_, _, pdb := agentFixture(t, prov, agentSeed{})

	if _, err := pdb.Writer().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create app table: %v", err)
	}
	if _, err := pdb.Writer().Exec(`CREATE TABLE of_custom (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create of_ table: %v", err)
	}

	names := agentTableNames(pdb)
	found := false
	for _, n := range names {
		if strings.HasPrefix(n, "of_") || strings.HasPrefix(n, "sqlite_") {
			t.Errorf("table list leaked a platform table: %q", n)
		}
		if n == "items" {
			found = true
		}
	}
	if !found {
		t.Errorf("table list = %v, want it to include the app table \"items\"", names)
	}
}

// TestAgentRunRequiresAuth: an auth-gated endpoint rejects an unauthenticated
// call before spending anything.
func TestAgentRunRequiresAuth(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{Content: "x"}}}
	h, project, pdb := agentFixture(t, prov, agentSeed{RequiresAuth: true})

	rec := postRun(t, h, project, pdb, `{"messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(prov.got) != 0 {
		t.Errorf("provider was called %d times on an unauthorized request, want 0", len(prov.got))
	}
}

// TestAgentRunDailyBudgetExhausted: a run is refused once the day's budget is
// spent, without calling the model.
func TestAgentRunDailyBudgetExhausted(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{Content: "x"}}}
	h, project, pdb := agentFixture(t, prov, agentSeed{DailyBudget: 5, DailyUsed: 5})

	rec := postRun(t, h, project, pdb, `{"messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "budget") {
		t.Errorf("body = %q, want a budget-exhausted message", rec.Body.String())
	}
	if len(prov.got) != 0 {
		t.Errorf("provider was called %d times over budget, want 0", len(prov.got))
	}
}

func TestToolNameForFunction(t *testing.T) {
	cases := map[string]string{
		"lookup_order":    "fn_lookup_order",
		"lookup-order/v2": "fn_lookup-order_v2",
		"a.b c":           "fn_a_b_c",
		"":                "fn_fn",
	}
	for in, want := range cases {
		if got := toolNameForFunction(in); got != want {
			t.Errorf("toolNameForFunction(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFunctionAllowlist(t *testing.T) {
	if got := parseFunctionAllowlist(`["a","b"]`); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("parseFunctionAllowlist = %v, want [a b]", got)
	}
	if got := parseFunctionAllowlist(""); got != nil {
		t.Errorf("empty allowlist = %v, want nil", got)
	}
	if got := parseFunctionAllowlist("not json"); got != nil {
		t.Errorf("malformed allowlist = %v, want nil", got)
	}
}

func toolOffered(defs []llm.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

// TestAgentRunSpawnsSubagent drives the endpoint through a delegated sub-agent:
// the model spawns one, it answers, the top agent finishes, and both are billed.
func TestAgentRunSpawnsSubagent(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{
		{ToolCalls: []llm.ToolCall{{ID: "s1", Name: "spawn_subagent", Arguments: `{"task":"do the subtask"}`}}, Usage: llm.Usage{InputTokens: 5, OutputTokens: 5}},
		{Content: "sub done", StopReason: "end", Usage: llm.Usage{InputTokens: 3, OutputTokens: 3}},
		{Content: "top done", StopReason: "end", Usage: llm.Usage{InputTokens: 2, OutputTokens: 2}},
	}}
	h, project, pdb := agentFixture(t, prov, agentSeed{MaxSubagents: 2, MaxDepth: 1})

	rec := postRun(t, h, project, pdb, `{"messages":[{"role":"user","content":"go"}]}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Content   string `json:"content"`
		Subagents int    `json:"subagents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Content != "top done" || got.Subagents != 1 {
		t.Errorf("got content=%q subagents=%d, want \"top done\"/1", got.Content, got.Subagents)
	}
	if !toolOffered(prov.got[0].Tools, "spawn_subagent") {
		t.Errorf("top request did not offer spawn_subagent: %+v", prov.got[0].Tools)
	}
	var used int
	if err := pdb.Reader().QueryRow(`SELECT daily_tokens_used FROM of_agent_endpoints WHERE path='assistant'`).Scan(&used); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if used != 20 {
		t.Errorf("daily_tokens_used = %d, want 20 (parent+sub aggregated)", used)
	}
}

// TestAgentRunStreamsSSE covers the streaming form: step frames then a terminal
// done frame carrying the final answer.
func TestAgentRunStreamsSSE(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "query_data", Arguments: `{"sql":"SELECT COUNT(*) AS n FROM items","params":[]}`}}, Usage: llm.Usage{InputTokens: 2, OutputTokens: 2}},
		{Content: "one item", StopReason: "end", Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	h, project, pdb := agentFixture(t, prov, agentSeed{AllowDataQuery: true})
	if _, err := pdb.Writer().Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := pdb.Writer().Exec(`INSERT INTO items (name) VALUES ('a')`); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/assistant/run?stream=1", strings.NewReader(`{"messages":[{"role":"user","content":"how many"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.apiAgent(rec, req, project, pdb, "assistant")

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream (body %s)", ct, rec.Body.String())
	}
	names := map[string]bool{}
	var doneData string
	for _, f := range parseSSE(rec.Body.String()) {
		names[f.event] = true
		if f.event == "done" {
			doneData = f.data
		}
	}
	for _, want := range []string{"tool_call", "tool_result", "done"} {
		if !names[want] {
			t.Errorf("missing SSE event %q; got %v", want, names)
		}
	}
	if !strings.Contains(doneData, "one item") {
		t.Errorf("done frame = %q, want the final content", doneData)
	}
}

func TestAgentHTTPFetchToolOffered(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{Content: "hi", StopReason: "end", Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}}}}
	h, project, pdb := agentFixture(t, prov, agentSeed{AllowHTTPFetch: true})
	rec := postRun(t, h, project, pdb, `{"messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if !toolOffered(prov.got[0].Tools, "http_fetch") {
		t.Errorf("http_fetch not offered: %+v", prov.got[0].Tools)
	}
}

type sseFrame struct{ event, data string }

// parseSSE splits an SSE body into its frames, joining multi-line data.
func parseSSE(body string) []sseFrame {
	var out []sseFrame
	for _, raw := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var f sseFrame
		for _, line := range strings.Split(raw, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data += strings.TrimPrefix(line, "data: ")
			}
		}
		if f.event != "" || f.data != "" {
			out = append(out, f)
		}
	}
	return out
}

// TestAgentRunAsync starts a detached run and polls it to completion, then checks
// the result and that usage was billed.
func TestAgentRunAsync(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{
		{Content: "async answer", StopReason: "end", Usage: llm.Usage{InputTokens: 4, OutputTokens: 2}},
	}}
	h, project, pdb := agentFixture(t, prov, agentSeed{})

	start := httptest.NewRequest(http.MethodPost, "/api/assistant/run?async=1", strings.NewReader(`{"messages":[{"role":"user","content":"go"}]}`))
	start.Header.Set("Content-Type", "application/json")
	srec := httptest.NewRecorder()
	h.apiAgent(srec, start, project, pdb, "assistant")
	if srec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202 (body %s)", srec.Code, srec.Body.String())
	}
	var started struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if started.RunID == "" || started.Status != "running" {
		t.Fatalf("start response = %+v, want a run_id and status running", started)
	}

	var poll struct {
		Status  string `json:"status"`
		Content string `json:"content"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		preq := httptest.NewRequest(http.MethodGet, "/api/assistant/run?id="+started.RunID, nil)
		prec := httptest.NewRecorder()
		h.apiAgentRunStatus(prec, preq, project, pdb, "assistant")
		if prec.Code != http.StatusOK {
			t.Fatalf("poll status = %d (body %s)", prec.Code, prec.Body.String())
		}
		poll.Status, poll.Content = "", ""
		if err := json.Unmarshal(prec.Body.Bytes(), &poll); err != nil {
			t.Fatalf("decode poll: %v", err)
		}
		if poll.Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async run did not finish within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if poll.Status != "done" || poll.Content != "async answer" {
		t.Errorf("final poll = %+v, want done/\"async answer\"", poll)
	}
	var used int
	if err := pdb.Reader().QueryRow(`SELECT daily_tokens_used FROM of_agent_endpoints WHERE path='assistant'`).Scan(&used); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if used != 6 {
		t.Errorf("daily_tokens_used = %d, want 6", used)
	}
}

func TestAgentRunStatusNotFound(t *testing.T) {
	h, project, pdb := agentFixture(t, &recordingProvider{replies: []llm.CompletionResponse{{Content: "x"}}}, agentSeed{})
	req := httptest.NewRequest(http.MethodGet, "/api/assistant/run?id=nope", nil)
	rec := httptest.NewRecorder()
	h.apiAgentRunStatus(rec, req, project, pdb, "assistant")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAgentRunStatusRequiresID(t *testing.T) {
	h, project, pdb := agentFixture(t, &recordingProvider{replies: []llm.CompletionResponse{{Content: "x"}}}, agentSeed{})
	req := httptest.NewRequest(http.MethodGet, "/api/assistant/run", nil)
	rec := httptest.NewRecorder()
	h.apiAgentRunStatus(rec, req, project, pdb, "assistant")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestAgentRunStreamViaGetIsGuided covers a page that streams with EventSource (a
// GET with messages in the query, no auth header). On an auth-gated agent that
// would otherwise 401 with no clue; the handler returns a 400 that names the fix
// (POST + fetch reader, not EventSource), BEFORE the auth gate.
func TestAgentRunStreamViaGetIsGuided(t *testing.T) {
	h, project, pdb := agentFixture(t, &recordingProvider{replies: []llm.CompletionResponse{{Content: "x"}}}, agentSeed{RequiresAuth: true})
	req := httptest.NewRequest(http.MethodGet, "/api/assistant/run?stream=1&messages=%5B%5D", nil)
	rec := httptest.NewRecorder()
	h.apiAgentRunStatus(rec, req, project, pdb, "assistant")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (actionable guidance, not a bare 401)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "POST") || !strings.Contains(body, "EventSource") {
		t.Errorf("body = %q, want guidance naming POST and EventSource", body)
	}
}
