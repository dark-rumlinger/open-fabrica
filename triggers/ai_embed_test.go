package triggers

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"openfabrica/events"
	"openfabrica/internal/vectorindex"
	"openfabrica/providers/llm"
	_ "openfabrica/tools/data" // registers manage_schema / manage_data / manage_vector_search
	"openfabrica/tools/tooltest"
)

func keywordEmbed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, s := range texts {
		if strings.Contains(strings.ToLower(s), "cat") {
			out[i] = []float32{1, 0}
		} else {
			out[i] = []float32{0, 1}
		}
	}
	return out, nil
}

// ai_embed on data.insert must leave the new row findable by
// manage_vector_search semantic_search. It must write where search reads, so an
// "embed rows on insert" trigger actually makes a row searchable.
func TestAIEmbedMakesRowSemanticallySearchable(t *testing.T) {
	h := tooltest.New(t)
	h.Env.EmbedFunc = keywordEmbed
	h.MustOK("manage_schema", map[string]any{
		"action": "create_table", "table_name": "notes",
		"column_defs": map[string]any{"body": "TEXT"},
	})
	h.MustOK("manage_vector_search", map[string]any{
		"action": "create_vector_index", "table_name": "notes", "embedding_columns": []any{"body"},
	})
	// Rows written after the index exists, the way a live app writes them.
	h.MustOK("manage_data", map[string]any{"action": "insert", "table_name": "notes",
		"rows": []any{map[string]any{"body": "my cat sleeps all day"}, map[string]any{"body": "quarterly revenue"}}})

	r := &Runner{llmRegistry: llm.NewRegistry(), globalDB: h.Global.DB, embedFn: keywordEmbed,
		logger: slog.New(slog.DiscardHandler)}
	for _, id := range []int64{1, 2} {
		// Zero config: the row and its text come from the event and the index.
		ev := events.Event{Type: events.EventDataInsert, ProjectID: 1, Payload: map[string]any{"table": "notes", "id": id}}
		if err := r.executeAIEmbed(h.Project.Writer(), ev, map[string]any{}); err != nil {
			t.Fatalf("ai_embed row %d: %v", id, err)
		}
	}

	res := h.MustOK("manage_vector_search", map[string]any{
		"action": "semantic_search", "table_name": "notes", "semantic_query": "cats",
	})
	if got := tooltest.DataJSON(res); !strings.Contains(got, "my cat sleeps") || strings.Contains(got, "revenue") {
		t.Fatalf("semantic_search over trigger-embedded rows = %s, want only the cat note", got)
	}
}

// Without a vector index there is nowhere to put the vector: say so plainly.
func TestAIEmbedRequiresVectorIndex(t *testing.T) {
	h := tooltest.New(t)
	h.MustOK("manage_schema", map[string]any{
		"action": "create_table", "table_name": "notes",
		"column_defs": map[string]any{"body": "TEXT"},
	})
	r := &Runner{llmRegistry: llm.NewRegistry(), globalDB: h.Global.DB, embedFn: keywordEmbed}
	ev := events.Event{Type: events.EventDataInsert, Payload: map[string]any{"table": "notes", "id": 1}}
	err := r.executeAIEmbed(h.Project.Writer(), ev, map[string]any{"text": "x"})
	if err == nil || !strings.Contains(err.Error(), vectorindex.ErrNoIndex.Error()) {
		t.Fatalf("err = %v, want the create_vector_index hint", err)
	}
}
