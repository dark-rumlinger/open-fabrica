package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"openfabrica/providers/llm"
)

// retrieveKnowledgeContext returns the top-K most relevant chunk texts for a
// query from a knowledge base, formatted as a single context block. The
// embedding + cosine ranking mirrors manage_vector_search's semantic_search so
// build-time retrieve and runtime retrieval rank identically. Returns "" on any
// miss (unknown base, no embeddings, embed failure) so the caller degrades to
// plain chat rather than failing the request.
func (h *Handler) retrieveKnowledgeContext(ctx context.Context, projectID int, readDB *sql.DB, baseName, query string, topK int) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}

	var table string
	if err := readDB.QueryRow("SELECT table_name FROM of_knowledge_bases WHERE name = ?", baseName).Scan(&table); err != nil {
		return ""
	}

	if h.deps.DB == nil {
		return ""
	}
	embeddings, err := llm.EmbedTexts(ctx, h.deps.DB.DB, h.deps.Encryptor, h.deps.LLMRegistry, []string{query})
	if err != nil || len(embeddings) == 0 {
		slog.Warn("knowledge retrieval: embed failed", "project_id", projectID, "base", baseName, "error", err)
		return ""
	}
	queryEmb := embeddings[0]

	// table comes from our own registry (sanitized at create_base), safe to use.
	rows, err := readDB.Query(fmt.Sprintf("SELECT text, _embedding FROM %s WHERE _embedding != '' AND _embedding IS NOT NULL", table))
	if err != nil {
		return ""
	}
	defer rows.Close()

	type scored struct {
		text string
		sim  float64
	}
	var ranked []scored
	for rows.Next() {
		var text, embStr string
		if rows.Scan(&text, &embStr) != nil || embStr == "" {
			continue
		}
		var emb []float32
		if json.Unmarshal([]byte(embStr), &emb) != nil {
			continue
		}
		ranked = append(ranked, scored{text: text, sim: cosineSim(queryEmb, emb)})
	}
	// A failed iteration degrades to plain chat, like any other miss.
	if rows.Err() != nil {
		return ""
	}
	if len(ranked) == 0 {
		return ""
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].sim > ranked[j].sim })
	if topK <= 0 {
		topK = 5
	}
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}

	var b strings.Builder
	for i, r := range ranked {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(r.text)
	}
	return b.String()
}

// cosineSim is the cosine similarity of two equal-length vectors, 0 when either
// is empty or their lengths differ.
func cosineSim(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
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
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
