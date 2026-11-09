package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"sync"
)

// Semantic ranking for the find_tool catalogue, shared by both discovery
// surfaces (the engine build pipeline and the live chat session) so the static
// catalogue is embedded once process-wide. Lives in the llm package because it
// needs an embedder and because keeping it provider-aware here lets the pure
// tools package stay free of any LLM dependency - the registry only does the
// rank-fusion math (Registry.HybridSearch), and the engine/chat layers hand it
// the semantic order this file computes.
//
// Every path is best-effort: find_tool(query) is a fallback, so a missing
// embeddings provider or a provider hiccup degrades to pure keyword search
// rather than failing the call.

// EmbedFn embeds a batch of texts into vectors in the same order. Callers bind
// it to EmbedTexts with their global DB, encryptor, and provider registry.
type EmbedFn func(ctx context.Context, texts []string) ([][]float32, error)

// toolVecEntry is one cached catalogue embedding set plus the vector the embedder gave a
// fixed probe text when the set was built. The probe is how a later call notices the
// embedder is not the one that built the set: a changed embedding model (or provider)
// keeps the catalogue text, and so the cache key, but moves every vector.
type toolVecEntry struct {
	vecs  map[string][]float32
	probe []float32
}

// toolVecCache memoizes catalogue embeddings process-wide, keyed by a content
// hash of the (name, text) set. The catalogue is static at runtime, so this is
// computed once per distinct gated subset and reused across every find_tool
// query; only the per-query vector is embedded each call. Keyed by content hash
// (not a bare flag) so a catalogue change invalidates stale vectors, and checked
// against a probe vector so an embedder change does too.
var (
	toolVecMu    sync.Mutex
	toolVecCache = map[string]toolVecEntry{}
)

const (
	// maxToolVecCacheEntries bounds toolVecCache. One entry exists per distinct gated
	// catalogue subset (stage and capability combination), so the bound is far above
	// any real count.
	maxToolVecCacheEntries = 16

	// embedProbeText is embedded alongside every query, in the same request, to
	// fingerprint the embedder.
	embedProbeText = "embedding fingerprint"
	// sameEmbedderSimilarity is how close two embeddings of the probe must be to
	// count as the same embedder. The same model returns the same vector, so this
	// only needs to clear floating-point and batching noise.
	sameEmbedderSimilarity = 0.99
	// minToolSimilarity is the least cosine similarity a tool needs to enter the
	// semantic ranking. Below it the match is noise: without a floor every tool
	// ranks, a search can never come back empty, and the miss guidance never
	// fires. Deliberately low: embedding models differ in baseline (some score
	// unrelated text high), and a model that does simply never filters, which
	// is today's behaviour. Keyword matches are unaffected.
	minToolSimilarity = 0.2
)

// RankToolsBySimilarity returns the tool names ranked by embedding similarity to
// query, most similar first, keeping only those above minToolSimilarity, or nil
// when embedding is unavailable or any step fails (the caller then falls back to
// pure keyword search). names and texts are parallel slices: texts[i] is the
// embedding input for names[i]. An empty, non-nil result means nothing in the
// catalogue is close to the query.
func RankToolsBySimilarity(ctx context.Context, embed EmbedFn, names, texts []string, query string) []string {
	if embed == nil || len(names) == 0 || len(names) != len(texts) {
		return nil
	}
	// One request carries the query and the embedder fingerprint.
	qv, err := embed(ctx, []string{query, embedProbeText})
	if err != nil || len(qv) != 2 || len(qv[0]) == 0 || len(qv[1]) == 0 {
		return nil
	}
	vecs := toolVectors(ctx, embed, names, texts, qv[1])
	if len(vecs) == 0 {
		return nil
	}

	type scored struct {
		name string
		sim  float32
	}
	ranked := make([]scored, 0, len(vecs))
	for name, v := range vecs {
		if sim := cosineSimilarity(qv[0], v); sim >= minToolSimilarity {
			ranked = append(ranked, scored{name: name, sim: sim})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].sim != ranked[j].sim {
			return ranked[i].sim > ranked[j].sim
		}
		return ranked[i].name < ranked[j].name
	})
	out := make([]string, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, s.name)
	}
	return out
}

// toolVectors returns the per-name catalogue embeddings, embedding the whole set
// once and caching it by content hash. A cached set is reused only while the
// embedder still answers the probe as it did when the set was built; otherwise it
// is rebuilt. Returns nil (not a partial map) on any failure so the caller cleanly
// falls back to keyword search. The embed call runs outside the cache lock so a
// slow provider can't block other callers.
func toolVectors(ctx context.Context, embed EmbedFn, names, texts []string, probe []float32) map[string][]float32 {
	hash := nameTextHash(names, texts)
	toolVecMu.Lock()
	cached, ok := toolVecCache[hash]
	toolVecMu.Unlock()
	if ok && cosineSimilarity(cached.probe, probe) >= sameEmbedderSimilarity {
		return cached.vecs
	}

	vecs, err := embed(ctx, texts)
	if err != nil || len(vecs) != len(names) {
		return nil
	}
	out := make(map[string][]float32, len(names))
	for i, n := range names {
		out[n] = vecs[i]
	}
	toolVecMu.Lock()
	// The key changes only when the catalogue text or the embedder does, so a handful
	// of entries covers every live variant; clearing past the bound keeps a catalogue
	// edit during development from growing the map for the life of the process.
	if len(toolVecCache) >= maxToolVecCacheEntries {
		clear(toolVecCache)
	}
	toolVecCache[hash] = toolVecEntry{vecs: out, probe: probe}
	toolVecMu.Unlock()
	return out
}

// nameTextHash fingerprints the (name, text) set so the embedding cache rebuilds
// only when the catalogue text changes and is shared across callers when it
// does not.
func nameTextHash(names, texts []string) string {
	h := sha256.New()
	for i, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write([]byte(texts[i]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cosineSimilarity returns the cosine similarity of two equal-length vectors,
// and 0 when the lengths differ or either has zero magnitude (which would
// otherwise divide to NaN).
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
