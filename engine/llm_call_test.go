package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
)

// noBackoff is a zero-delay backoff strategy.
func noBackoff(_ int) time.Duration { return 0 }

func testCaller(t *testing.T) (*llmCaller, *db.ProjectDB) {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	return &llmCaller{
		limiter: newLLMLimiter(2),
		pdb:     pdb,
		logger:  slog.Default(),
	}, pdb
}

func callerParams(p *enginetest.Provider) callParams {
	return callParams{
		rp:      resolvedProvider{Provider: p, Model: "fake-model", ProviderID: 1},
		req:     sampleRequest(),
		policy:  policyBuild,
		timeout: 5 * time.Second,
		stage:   "build",
		runID:   "run_test",
		served:  map[string]bool{},
	}
}

// TestReplayCacheHitOncePerSegment: an identical second request replays from of_llm_cache
// without touching the provider - but only once per segment (served set).
func TestReplayCacheHitOncePerSegment(t *testing.T) {
	c, _ := testCaller(t)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("first answer"),
		enginetest.Text("provider answer 2"),
	}}

	params := callerParams(p)
	resp, _, hit, err := c.call(context.Background(), params)
	if err != nil || hit {
		t.Fatalf("first call: hit=%v err=%v", hit, err)
	}
	if resp.Content != "first answer" {
		t.Fatalf("unexpected content %q", resp.Content)
	}
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.CallCount())
	}

	// Same request, same segment: served from cache, provider untouched.
	resp2, _, hit2, err := c.call(context.Background(), params)
	if err != nil || !hit2 {
		t.Fatalf("second call should hit cache: hit=%v err=%v", hit2, err)
	}
	if resp2.Content != "first answer" {
		t.Fatalf("cached content = %q", resp2.Content)
	}
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (cache hit)", p.CallCount())
	}

	// Third identical request in the SAME segment: served set blocks a second
	// replay, the provider answers (breaking deterministic failure loops).
	resp3, _, hit3, err := c.call(context.Background(), params)
	if err != nil || hit3 {
		t.Fatalf("third call must skip cache: hit=%v err=%v", hit3, err)
	}
	if resp3.Content != "provider answer 2" {
		t.Fatalf("third content = %q", resp3.Content)
	}
	if p.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", p.CallCount())
	}
}

// TestReplayCacheNewSegmentReplays: a fresh segment (fresh served set, the
// crash-resume case) replays the cached response for free.
func TestReplayCacheNewSegmentReplays(t *testing.T) {
	c, _ := testCaller(t)
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("expensive answer")}}

	params := callerParams(p)
	if _, _, _, err := c.call(context.Background(), params); err != nil {
		t.Fatal(err)
	}

	// "Restart": new segment = new served map.
	params.served = map[string]bool{}
	resp, _, hit, err := c.call(context.Background(), params)
	if err != nil || !hit {
		t.Fatalf("resume call should replay: hit=%v err=%v", hit, err)
	}
	if resp.Content != "expensive answer" || p.CallCount() != 1 {
		t.Fatalf("resume replay broken: content=%q calls=%d", resp.Content, p.CallCount())
	}
}

// TestReplayCacheDiscard: a response the caller could not use is evicted, so the
// identical request reaches the provider again. Without it, a response that is
// unusable for structural reasons - one the same request would only produce
// again - replays out of the cache and fails identically on every later run.
// This is the counterpart to TestReplayCacheNewSegmentReplays: a KEPT response
// replays in a fresh segment, a discarded one cannot.
func TestReplayCacheDiscard(t *testing.T) {
	c, _ := testCaller(t)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("unusable answer"), enginetest.Text("fresh answer"),
	}}

	params := callerParams(p)
	_, hash, _, err := c.call(context.Background(), params)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if hash == "" {
		t.Fatal("call returned no cache key, so a caller cannot discard the response")
	}

	c.cacheDiscard([]string{hash})

	// Fresh segment: a kept response would replay here for free.
	params.served = map[string]bool{}
	resp, _, hit, err := c.call(context.Background(), params)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if hit {
		t.Fatal("discarded response still replayed from cache")
	}
	if resp.Content != "fresh answer" || p.CallCount() != 2 {
		t.Fatalf("provider not re-asked: content=%q calls=%d", resp.Content, p.CallCount())
	}
}

// TestReplayCacheOptOut: NoReplayCache (LIVE chat) always reaches the provider.
func TestReplayCacheOptOut(t *testing.T) {
	c, _ := testCaller(t)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("a"), enginetest.Text("b"),
	}}
	params := callerParams(p)
	params.noReplayCache = true

	c.call(context.Background(), params)
	resp, _, hit, err := c.call(context.Background(), params)
	if err != nil || hit {
		t.Fatalf("opt-out call hit cache: hit=%v err=%v", hit, err)
	}
	if resp.Content != "b" || p.CallCount() != 2 {
		t.Fatalf("opt-out broken: content=%q calls=%d", resp.Content, p.CallCount())
	}
}

// TestNoReplayCacheSkipsWrite: a caller that never reads the replay cache must
// not write it either; the row could only ever serve a different caller whose
// request happens to hash identically.
func TestNoReplayCacheSkipsWrite(t *testing.T) {
	c, pdb := testCaller(t)
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("a")}}
	params := callerParams(p)
	params.noReplayCache = true
	if _, _, _, err := c.call(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_llm_cache").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("of_llm_cache rows = %d, want 0 for a NoReplayCache caller", n)
	}
}

// TestReplayCacheSkipsImages: requests carrying images are never cached.
func TestReplayCacheSkipsImages(t *testing.T) {
	c, _ := testCaller(t)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("a"), enginetest.Text("b"),
	}}
	params := callerParams(p)
	params.req.Messages[0].Images = []llm.MessageImage{{MIMEType: "image/png", Data: []byte{1, 2, 3}}}

	c.call(context.Background(), params)
	_, _, hit, err := c.call(context.Background(), params)
	if err != nil || hit {
		t.Fatalf("image request hit cache: hit=%v err=%v", hit, err)
	}
	if p.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", p.CallCount())
	}
}

// TestRetryClassification: transient errors retry within budget; fatal errors
// do not retry.
func TestRetryClassification(t *testing.T) {
	c, _ := testCaller(t)

	// Transient server error then success: retried.
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Fail(&llm.Error{Code: llm.CodeServer, Message: "boom"}),
		enginetest.Text("recovered"),
	}}
	params := callerParams(p)
	params.policy = StagePolicy{
		Transient: Policy{MaxAttempts: 3, Backoff: noBackoff},
		RateLimit: Policy{MaxAttempts: 2, Backoff: noBackoff},
		Timeout:   Policy{MaxAttempts: 2, Backoff: noBackoff},
	}
	resp, _, _, err := c.call(context.Background(), params)
	if err != nil {
		t.Fatalf("transient should recover: %v", err)
	}
	if resp.Content != "recovered" || p.CallCount() != 2 {
		t.Fatalf("retry broken: content=%q calls=%d", resp.Content, p.CallCount())
	}

	// Auth error: fatal, exactly one attempt. Distinct request content so the
	// transient case's cached response (same caller DB) can't replay here.
	p2 := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Fail(&llm.Error{Code: llm.CodeAuth, Message: "bad key"}),
		enginetest.Text("never"),
	}}
	params2 := callerParams(p2)
	params2.req.Messages[0].Content = "a different request entirely"
	params2.policy = params.policy
	if _, _, _, err := c.call(context.Background(), params2); err == nil {
		t.Fatal("auth error must surface")
	}
	if p2.CallCount() != 1 {
		t.Fatalf("auth error retried: calls=%d", p2.CallCount())
	}
	if !isPermanentError(&llm.Error{Code: llm.CodeAuth}) {
		t.Fatal("auth not classified permanent")
	}
}

// captureLogger records every of_llm_calls entry the caller would persist.
type captureLogger struct{ entries []llm.LLMLogEntry }

func (c *captureLogger) LogLLMCall(entry llm.LLMLogEntry) { c.entries = append(c.entries, entry) }

// TestCallLogsProviderName pins the field every per-model cost lookup keys on.
// of_llm_calls.provider_name is what CacheCostFactorsFor resolves a model's real
// cache ratios from (engine/run_log.go, providers/llm/budget.go,
// server/admin/builds_cost.go); an empty one silently prices every call from the
// crude transport estimate instead, so every call must write it.
func TestCallLogsProviderName(t *testing.T) {
	c, _ := testCaller(t)
	log := &captureLogger{}
	c.dbLog = log

	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("answer")}}
	if _, _, _, err := c.call(context.Background(), callerParams(p)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(log.entries) != 1 {
		t.Fatalf("got %d log entries, want 1", len(log.entries))
	}
	got := log.entries[0]
	if got.ProviderName != "enginetest" {
		t.Errorf("provider_name = %q, want %q", got.ProviderName, "enginetest")
	}
	if got.ProviderType != p.Type() {
		t.Errorf("provider_type = %q, want %q", got.ProviderType, p.Type())
	}
	if got.Model != "fake-model" {
		t.Errorf("model = %q, want %q", got.Model, "fake-model")
	}
}

// TestCallLogsFallbackProvider: when a swap answers, the row must name the
// provider that BILLED the call, not the one resolution started from. Attributing
// it to the primary would price the call from the wrong vendor's ratios.
func TestCallLogsFallbackProvider(t *testing.T) {
	c, _ := testCaller(t)
	log := &captureLogger{}
	c.dbLog = log

	primary := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Fail(&llm.Error{Code: llm.CodeServer, Message: "primary down"}),
	}}
	alt := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("fallback answer")}}

	p := callerParams(primary)
	p.policy = StagePolicy{
		Transient: Policy{MaxAttempts: 1, Backoff: noBackoff},
		RateLimit: Policy{MaxAttempts: 1, Backoff: noBackoff},
		Timeout:   Policy{MaxAttempts: 1, Backoff: noBackoff},
	}
	p.fallback = func(error) (llm.Provider, string) { return alt, "alt-model" }

	if _, _, _, err := c.call(context.Background(), p); err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(log.entries) != 1 {
		t.Fatalf("got %d log entries, want 1", len(log.entries))
	}
	got := log.entries[0]
	if got.ProviderName != alt.Name() {
		t.Errorf("provider_name = %q, want the fallback %q", got.ProviderName, alt.Name())
	}
	if got.Model != "alt-model" {
		t.Errorf("model = %q, want the fallback model %q", got.Model, "alt-model")
	}
}
