package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/replayoutbox"
	"openfabrica/providers/llm"
)

// recordingProvider answers completions from a scripted queue and keeps every
// request it was handed, so a test can assert on what actually went on the wire.
type recordingProvider struct {
	replies []llm.CompletionResponse
	got     []llm.CompletionRequest
}

func (p *recordingProvider) Name() string                       { return "rec" }
func (p *recordingProvider) Type() string                       { return "openai-chat" }
func (p *recordingProvider) Ping(context.Context, string) error { return nil }
func (p *recordingProvider) SupportsStructuredOutput() bool     { return true }

func (p *recordingProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.got = append(p.got, req)
	resp := p.replies[min(len(p.got)-1, len(p.replies)-1)]
	return &resp, nil
}

func (p *recordingProvider) Stream(ctx context.Context, req llm.CompletionRequest, cb llm.StreamCallback) error {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return err
	}
	cb(llm.StreamChunk{Delta: resp.Content, Usage: &resp.Usage})
	cb(llm.StreamChunk{Done: true, StopReason: resp.StopReason, Usage: &resp.Usage})
	return nil
}

// llmProxyFixture stands up a project with one schema-constrained LLM endpoint
// wired to a scripted provider, and returns everything the assertions need.
func llmProxyFixture(t *testing.T, prov llm.Provider, responseSchema string) (*Handler, *models.Project, *db.ProjectDB) {
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
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_llm_endpoints (path, system_prompt, max_tokens, response_schema, daily_token_budget)
		 VALUES ('generate', 'be useful', 4096, ?, 0)`, responseSchema); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}

	h := &Handler{deps: &Deps{DB: globalDB, ProjectDBManager: mgr, LLMRegistry: llm.NewRegistry()}}
	h.resolveModel = func(int) (llm.Provider, string, error) { return prov, "test-model", nil }
	project, err := models.GetProjectByID(globalDB.DB, 7)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	return h, project, pdb
}

func postComplete(t *testing.T, h *Handler, project *models.Project, pdb *db.ProjectDB, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/generate/complete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.apiLLM(rec, req, project, pdb, "generate", "complete")
	return rec
}

// TestLLMProxyPinsSchemaRailsAndStrict covers what reaches the provider for an
// endpoint whose stored schema is the shape a build actually authors: no
// additionalProperties anywhere. The proxy must pin the rail and, once every
// property is both declared and required, ask for strict enforcement rather
// than shipping the loosest possible document as a hint.
func TestLLMProxyPinsSchemaRailsAndStrict(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{
		Content:    `{"html":"<p>hi</p>","css":"p{}"}`,
		StopReason: "end",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 20},
	}}}
	stored := `{"type":"object","properties":{"html":{"type":"string"},"css":{"type":"string"}},"required":["html","css"]}`
	h, project, pdb := llmProxyFixture(t, prov, stored)

	rec := postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"a bakery site"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(prov.got) != 1 {
		t.Fatalf("provider saw %d calls, want 1", len(prov.got))
	}
	sent := prov.got[0].ResponseSchema
	if sent == nil {
		t.Fatal("no schema was pinned on the request")
	}
	if !strings.Contains(string(sent.Schema), `"additionalProperties":false`) {
		t.Errorf("rail not pinned on the wire schema: %s", sent.Schema)
	}
	if !sent.Strict {
		t.Error("strict was not pinned for a schema that satisfies the strict rules")
	}
}

// TestLLMProxySetsCacheRouting: every proxy call carries a per-endpoint
// PromptCacheKey so an endpoint's traffic lands on one cache node, and
// CacheSystem rides only KB-less endpoints (a RAG-bearing system prompt is
// per-request, so a cache write could never be re-read).
func TestLLMProxySetsCacheRouting(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{
		Content: "ok", StopReason: "end", Usage: llm.Usage{InputTokens: 1, OutputTokens: 1},
	}}}
	h, project, pdb := llmProxyFixture(t, prov, "")

	rec := postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	sent := prov.got[0]
	if !sent.CacheSystem {
		t.Error("a KB-less endpoint should set CacheSystem")
	}
	if sent.PromptCacheKey != "proxy:7:generate" {
		t.Errorf("PromptCacheKey = %q, want proxy:7:generate", sent.PromptCacheKey)
	}

	// A knowledge-base endpoint keeps the routing key but not the system write.
	if _, err := pdb.Writer().Exec(`UPDATE of_llm_endpoints SET knowledge_base = 'kb' WHERE path = 'generate'`); err != nil {
		t.Fatalf("set knowledge_base: %v", err)
	}
	rec = postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("kb status = %d (body %s)", rec.Code, rec.Body.String())
	}
	sent = prov.got[len(prov.got)-1]
	if sent.CacheSystem {
		t.Error("a KB-bearing endpoint must not mark its per-request system prompt cacheable")
	}
	if sent.PromptCacheKey != "proxy:7:generate" {
		t.Errorf("kb PromptCacheKey = %q, want proxy:7:generate", sent.PromptCacheKey)
	}
}

// TestLLMProxyLogsCallToProjectDB locks the forensics: a live-mode call lands in
// of_llm_calls the way an engine call does, tagged with its endpoint, so a
// schema miss can be diagnosed after the fact from the reply that caused it.
func TestLLMProxyLogsCallToProjectDB(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{
		Content:    `{"html":"<p>hi</p>","css":"p{}"}`,
		StopReason: "end",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 20},
	}}}
	stored := `{"type":"object","properties":{"html":{"type":"string"},"css":{"type":"string"}},"required":["html","css"]}`
	h, project, pdb := llmProxyFixture(t, prov, stored)

	if rec := postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"a bakery site"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var source, session, content, stop string
	var in, out int
	err := pdb.Reader().QueryRow(
		`SELECT source, session_id, response_content, response_stop_reason, input_tokens, output_tokens
		   FROM of_llm_calls ORDER BY id DESC LIMIT 1`,
	).Scan(&source, &session, &content, &stop, &in, &out)
	if err == sql.ErrNoRows {
		t.Fatal("the proxy call was not logged to of_llm_calls")
	}
	if err != nil {
		t.Fatalf("read log row: %v", err)
	}
	if source != "proxy" || session != "generate" {
		t.Errorf("source/session = %q/%q, want proxy/generate", source, session)
	}
	if !strings.Contains(content, "<p>hi</p>") || stop != "end" {
		t.Errorf("reply not captured: content=%q stop=%q", content, stop)
	}
	if in != 10 || out != 20 {
		t.Errorf("usage not captured: in=%d out=%d", in, out)
	}

	// The same call must move the endpoint's daily counter, a write that must go
	// through the write pool, not the read pool.
	var used int
	if err := pdb.Reader().QueryRow(`SELECT daily_tokens_used FROM of_llm_endpoints WHERE path='generate'`).Scan(&used); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if used != 30 {
		t.Errorf("daily_tokens_used = %d, want 30", used)
	}
}

// TestLLMProxySchemaMissRetriesWithSample covers the corrective turn: a reply of
// the wrong shape is retried once, and the retry prompt names the schema and
// quotes what came back, so the model is told what it did rather than only that
// it was wrong.
func TestLLMProxySchemaMissRetriesWithSample(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{
		{Content: "<html>a whole page, not JSON</html>", StopReason: "end", Usage: llm.Usage{OutputTokens: 5}},
		{Content: `{"html":"<p>ok</p>","css":"p{}"}`, StopReason: "end", Usage: llm.Usage{OutputTokens: 6}},
	}}
	stored := `{"type":"object","properties":{"html":{"type":"string"},"css":{"type":"string"}},"required":["html","css"]}`
	h, project, pdb := llmProxyFixture(t, prov, stored)

	rec := postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"a bakery site"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a successful retry (body %s)", rec.Code, rec.Body.String())
	}
	if len(prov.got) != 2 {
		t.Fatalf("provider saw %d calls, want 2 (original + corrective)", len(prov.got))
	}
	corrective := prov.got[1].Messages[len(prov.got[1].Messages)-1]
	if corrective.Role != llm.RoleUser {
		t.Errorf("corrective turn role = %q, want user", corrective.Role)
	}
	if !strings.Contains(corrective.Content, "a whole page, not JSON") {
		t.Errorf("corrective turn does not quote the rejected reply: %q", corrective.Content)
	}
	if !strings.Contains(corrective.Content, `"required"`) {
		t.Errorf("corrective turn does not spell out the schema: %q", corrective.Content)
	}

	var body struct {
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	got := body.Content
	if got == "" {
		got = body.Data.Content
	}
	if !strings.Contains(got, "<p>ok</p>") {
		t.Errorf("client got %q, want the retry's reply", got)
	}

	// Both calls are billed and both are logged.
	var calls, used int
	if err := pdb.Reader().QueryRow(`SELECT COUNT(*) FROM of_llm_calls WHERE source='proxy'`).Scan(&calls); err != nil {
		t.Fatalf("count log rows: %v", err)
	}
	if calls != 2 {
		t.Errorf("logged %d proxy calls, want 2", calls)
	}
	if err := pdb.Reader().QueryRow(`SELECT daily_tokens_used FROM of_llm_endpoints WHERE path='generate'`).Scan(&used); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	if used != 11 {
		t.Errorf("daily_tokens_used = %d, want 11 (both calls billed)", used)
	}
}

// While a user-flow replay of the project runs, the endpoint answers from the stand-in:
// the real provider is never called, and a schema-constrained endpoint still gets a reply
// of the right shape, so the app's own handling of it runs.
func TestLLMProxyAnswersFromTheStandInDuringAReplay(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{Content: "real", StopReason: "end"}}}
	stored := `{"type":"object","properties":{"html":{"type":"string"},"css":{"type":"string"}},"required":["html","css"]}`
	h, project, pdb := llmProxyFixture(t, prov, stored)

	end := replayoutbox.Begin(project.ID)
	rec := postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"a bakery site"}]}`)
	end()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(prov.got) != 0 {
		t.Fatalf("the real provider must not be called during a replay, saw %d calls", len(prov.got))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `html`) || !strings.Contains(rec.Body.String(), `css`) {
		t.Errorf("the reply must carry the schema's fields: %s", rec.Body.String())
	}

	// Outside a replay the real provider answers again.
	if rec := postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"a bakery site"}]}`); rec.Code == http.StatusOK && len(prov.got) != 1 {
		t.Fatalf("after the replay the provider must be used, saw %d calls", len(prov.got))
	}
}

// TestLLMProxyHonoursThePublicPool pins the project-level public AI pool: once the
// project's proxy/agent spend reaches its public token budget, the endpoint answers
// 429 before calling the model, even though the endpoint's own budget is unlimited.
func TestLLMProxyHonoursThePublicPool(t *testing.T) {
	prov := &recordingProvider{replies: []llm.CompletionResponse{{Content: `{"html":"x","css":"y"}`, StopReason: "end"}}}
	h, project, pdb := llmProxyFixture(t, prov, "")
	if _, err := pdb.Writer().Exec(`INSERT INTO of_spend_limits (id, daily_public_token_budget) VALUES (1, 100)`); err != nil {
		t.Fatalf("seed limit: %v", err)
	}
	if _, err := pdb.Writer().Exec(`INSERT INTO of_llm_calls (source, model, input_tokens, output_tokens) VALUES ('agent', 'test-model', 80, 40)`); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	rec := postComplete(t, h, project, pdb, `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the public pool is spent (body %s)", rec.Code, rec.Body.String())
	}
	if len(prov.got) != 0 {
		t.Fatal("the model was called although the public pool was spent")
	}
}
