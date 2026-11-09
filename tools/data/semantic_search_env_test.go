package data

import (
	"context"
	"strings"
	"testing"

	"openfabrica/tools/tooltest"
)

// fakeEmbed maps text onto a 2-d vector by keyword so similarity is predictable:
// anything mentioning "cat" points one way, everything else the other.
func fakeEmbed(_ context.Context, texts []string) ([][]float32, error) {
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

// TestSemanticSearchThroughEnv pins the embeddings wiring: the EmbedFunc set on
// the shared tools.Env must reach the ToolContext of a real Invoke call. It used
// to be set on no production call path, so embed and semantic_search always
// failed with "no embedding provider configured".
func TestSemanticSearchThroughEnv(t *testing.T) {
	h := tooltest.New(t)
	h.Env.EmbedFunc = fakeEmbed

	h.MustOK("manage_schema", map[string]any{
		"action": "create_table", "table_name": "notes",
		"column_defs": map[string]any{"body": "TEXT"},
	})
	h.MustOK("manage_data", map[string]any{
		"action": "insert", "table_name": "notes",
		"rows": []any{
			map[string]any{"body": "my cat sleeps all day"},
			map[string]any{"body": "quarterly revenue report"},
		},
	})
	h.MustOK("manage_vector_search", map[string]any{
		"action": "create_vector_index", "table_name": "notes",
		"embedding_columns": []any{"body"},
	})
	h.MustOK("manage_vector_search", map[string]any{"action": "embed", "table_name": "notes"})

	res := h.MustOK("manage_vector_search", map[string]any{
		"action": "semantic_search", "table_name": "notes", "semantic_query": "cats",
	})
	got := tooltest.DataJSON(res)
	if !strings.Contains(got, "my cat sleeps") || strings.Contains(got, "revenue") {
		t.Fatalf("semantic_search should return only the cat note, got %s", got)
	}
}

// TestSemanticSearchWithoutEmbedderFailsCleanly keeps the no-provider path a
// clear tool failure rather than a panic.
func TestSemanticSearchWithoutEmbedderFailsCleanly(t *testing.T) {
	h := tooltest.New(t)
	h.MustOK("manage_schema", map[string]any{
		"action": "create_table", "table_name": "notes",
		"column_defs": map[string]any{"body": "TEXT"},
	})
	res := h.MustFail("manage_vector_search", map[string]any{
		"action": "semantic_search", "table_name": "notes", "semantic_query": "cats",
	}, "")
	if !strings.Contains(res.Error, "no embedding provider") {
		t.Fatalf("error = %q, want the no-embedding-provider message", res.Error)
	}
}

// The vector searches return whole rows to the model, so they must drop PASSWORD and
// ENCRYPTED columns exactly as the query action does. They used to SELECT * and strip
// only the embedding, which put a bcrypt hash into the model's context.
func TestVectorSearchesOmitSecureColumns(t *testing.T) {
	h := tooltest.New(t)
	h.Env.EmbedFunc = fakeEmbed

	h.MustOK("manage_schema", map[string]any{
		"action": "create_table", "table_name": "people",
		"column_defs": map[string]any{"bio": "TEXT", "password": "PASSWORD", "token": "ENCRYPTED"},
	})
	h.MustOK("manage_data", map[string]any{
		"action": "insert", "table_name": "people",
		"data": map[string]any{"bio": "owns a cat", "password": "hunter2-secret", "token": "tok-plain-value"},
	})
	h.MustOK("manage_vector_search", map[string]any{
		"action": "create_vector_index", "table_name": "people",
		"embedding_columns": []any{"bio"},
	})
	h.MustOK("manage_vector_search", map[string]any{"action": "embed", "table_name": "people"})

	for _, action := range []string{"semantic_search", "hybrid_search"} {
		res := h.MustOK("manage_vector_search", map[string]any{
			"action": action, "table_name": "people", "semantic_query": "cats",
		})
		got := tooltest.DataJSON(res)
		if !strings.Contains(got, "owns a cat") {
			t.Fatalf("%s: expected the row back, got %s", action, got)
		}
		for _, secret := range []string{"password", "token", "$2a$", "tok-plain-value", "hunter2"} {
			if strings.Contains(got, secret) {
				t.Fatalf("%s leaked a secure column (%q): %s", action, secret, got)
			}
		}
	}
}
