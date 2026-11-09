package llm

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// fakeEmbedder maps known texts to fixed vectors and counts how many texts it
// has embedded, so a test can assert the catalogue is embedded once and cached.
type fakeEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	calls   int       // number of texts embedded across all invocations
	probe   []float32 // what the embedder answers for the fingerprint probe; nil is a fixed default
}

func (f *fakeEmbedder) embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		f.calls++
		if t == embedProbeText {
			if f.probe != nil {
				out[i] = f.probe
			} else {
				out[i] = []float32{1, 0, 0, 0}
			}
			continue
		}
		v, ok := f.vectors[t]
		if !ok {
			return nil, fmt.Errorf("no vector for %q", t)
		}
		out[i] = v
	}
	return out, nil
}

func TestRankToolsBySimilarity_OrdersByCosine(t *testing.T) {
	fe := &fakeEmbedder{vectors: map[string][]float32{
		"email-text": {1, 0, 0},
		"sms-text":   {0, 1, 0},
		"feed-text":  {0, 0, 1},
		"query":      {0.9, 0.4, 0}, // closest to email, then sms; unrelated to feed
	}}
	names := []string{"manage_email", "manage_sms", "manage_feeds"}
	texts := []string{"email-text", "sms-text", "feed-text"}

	order := RankToolsBySimilarity(context.Background(), fe.embed, names, texts, "query")
	// manage_feeds is orthogonal to the query: below the floor, so it is left out.
	want := []string{"manage_email", "manage_sms"}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("rank %d = %q, want %q (full: %v)", i, order[i], want[i], order)
		}
	}
}

func TestRankToolsBySimilarity_CachesCatalogueVectors(t *testing.T) {
	// Distinct text set so this test's cache entry can't collide with another's.
	fe := &fakeEmbedder{vectors: map[string][]float32{
		"cache-a": {1, 0},
		"cache-b": {0, 1},
		"q1":      {1, 0},
		"q2":      {0, 1},
	}}
	names := []string{"tool_a", "tool_b"}
	texts := []string{"cache-a", "cache-b"}

	_ = RankToolsBySimilarity(context.Background(), fe.embed, names, texts, "q1")
	afterFirst := fe.calls // query + probe, then 2 catalogue = 4
	_ = RankToolsBySimilarity(context.Background(), fe.embed, names, texts, "q2")
	afterSecond := fe.calls

	// Second call must reuse cached catalogue vectors: only the query and probe embed.
	if afterFirst != 4 {
		t.Fatalf("first call embedded %d texts, want 4", afterFirst)
	}
	if afterSecond-afterFirst != 2 {
		t.Fatalf("second call embedded %d texts, want 2 (catalogue should be cached)", afterSecond-afterFirst)
	}
}

func TestRankToolsBySimilarity_DegradesToNil(t *testing.T) {
	fe := &fakeEmbedder{vectors: map[string][]float32{"x": {1}}}
	ctx := context.Background()
	if got := RankToolsBySimilarity(ctx, nil, []string{"a"}, []string{"x"}, "q"); got != nil {
		t.Fatalf("nil embedder should yield nil, got %v", got)
	}
	if got := RankToolsBySimilarity(ctx, fe.embed, nil, nil, "q"); got != nil {
		t.Fatalf("empty names should yield nil, got %v", got)
	}
	if got := RankToolsBySimilarity(ctx, fe.embed, []string{"a", "b"}, []string{"x"}, "q"); got != nil {
		t.Fatalf("mismatched name/text lengths should yield nil, got %v", got)
	}
	// Embed failure (unknown query text) must degrade to nil, not panic.
	if got := RankToolsBySimilarity(ctx, fe.embed, []string{"a"}, []string{"x"}, "unknown-query"); got != nil {
		t.Fatalf("embed failure should yield nil, got %v", got)
	}
}

// A query nothing in the catalogue resembles yields an empty, non-nil ranking, so the
// caller's keyword path can come back empty and the miss guidance can fire.
func TestRankToolsBySimilarity_NothingCloseIsEmptyNotNil(t *testing.T) {
	fe := &fakeEmbedder{vectors: map[string][]float32{
		"floor-a": {1, 0, 0}, "floor-b": {0, 1, 0}, "far-query": {0, 0, 1},
	}}
	got := RankToolsBySimilarity(context.Background(), fe.embed, []string{"fa", "fb"}, []string{"floor-a", "floor-b"}, "far-query")
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v, want an empty non-nil ranking", got)
	}
}

// Vectors cached under one embedding model must not be scored against a query from
// another, even when the catalogue text (the cache key) is unchanged and the
// dimensions happen to match.
func TestRankToolsBySimilarity_EmbedderChangeRebuildsCache(t *testing.T) {
	names := []string{"swap_a", "swap_b"}
	texts := []string{"swap-text-a", "swap-text-b"}
	oldModel := &fakeEmbedder{vectors: map[string][]float32{
		"swap-text-a": {1, 0}, "swap-text-b": {0, 1}, "swap-q": {1, 0},
	}}
	if got := RankToolsBySimilarity(context.Background(), oldModel.embed, names, texts, "swap-q"); len(got) == 0 || got[0] != "swap_a" {
		t.Fatalf("old model ranking = %v, want swap_a first", got)
	}

	// A different model: same dimensions, different geometry, different probe answer.
	newModel := &fakeEmbedder{probe: []float32{0, 1, 0, 0}, vectors: map[string][]float32{
		"swap-text-a": {0, 1}, "swap-text-b": {1, 0}, "swap-q": {1, 0},
	}}
	before := newModel.calls
	got := RankToolsBySimilarity(context.Background(), newModel.embed, names, texts, "swap-q")
	if len(got) == 0 || got[0] != "swap_b" {
		t.Fatalf("new model ranking = %v, want swap_b first (stale vectors were reused)", got)
	}
	if newModel.calls-before != 4 {
		t.Errorf("the new embedder embedded %d texts, want 4 (query, probe, and the rebuilt catalogue)", newModel.calls-before)
	}
}
