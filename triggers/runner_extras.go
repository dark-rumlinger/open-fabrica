package triggers

// Provider-backed action executors: send_push, llm_call, ai_embed.
// Each takes a project DB writer and a resolved-template config map and
// returns an error. The heavy provider lifting lives next to its tool, so
// this file is mostly thin glue.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"openfabrica/events"
	"openfabrica/internal/actionexec"
	"openfabrica/internal/vectorindex"
	"openfabrica/providers/llm"
	"openfabrica/security"
	"openfabrica/store"
)

// send_push

func (r *Runner) executeSendPush(projectDB *sql.DB, projectID int, config map[string]any) error {
	args := map[string]any{"action": "send"}
	if v, ok := config["endpoint_path"]; ok {
		args["endpoint_path"] = v
	}
	if v, ok := config["title"]; ok {
		args["title"] = v
	}
	// The trigger config uses "body"; the tool's send arg is "message_body".
	if v, ok := config["body"]; ok {
		args["message_body"] = v
	}
	if v, ok := config["url"]; ok {
		args["url"] = v
	}
	if v, ok := config["user_id"]; ok {
		args["user_id"] = v
	}
	return r.runTool(projectID, "manage_push", args)
}

// llm_call

func (r *Runner) executeLLMCall(projectDB *sql.DB, ev events.Event, config map[string]any) error {
	prompt, _ := config["prompt"].(string)
	if prompt == "" {
		return fmt.Errorf("llm_call: 'prompt' is required")
	}
	system, _ := config["system"].(string)
	maxTokens := 512
	if v, ok := config["max_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	}

	content, err := actionexec.Complete(r.lifecycle(), r.completeDeps(), ev.ProjectID, projectDB, "trigger", actionexec.CompleteRequest{
		Prompt: prompt, System: system, MaxTokens: maxTokens, Timeout: 90 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("llm_call: %w", err)
	}

	if table, _ := config["save_to_table"].(string); table != "" {
		col, _ := config["save_to_column"].(string)
		if col == "" {
			return fmt.Errorf("llm_call: save_to_column is required when save_to_table is set")
		}
		if !validIdentifier.MatchString(table) || !validIdentifier.MatchString(col) {
			return fmt.Errorf("llm_call: invalid table/column identifier")
		}
		where, _ := config["save_where"].(map[string]any)
		if len(where) == 0 {
			return fmt.Errorf("llm_call: save_where is required when save_to_table is set")
		}
		vals := []any{content}
		clauses := []string{}
		for c, v := range where {
			if !validIdentifier.MatchString(c) {
				return fmt.Errorf("llm_call: invalid where column %q", c)
			}
			clauses = append(clauses, security.QuoteIdent(c)+" = ?")
			vals = append(vals, v)
		}
		q := fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s", security.QuoteIdent(table), security.QuoteIdent(col), strings.Join(clauses, " AND "))
		if _, err := projectDB.Exec(q, vals...); err != nil {
			return fmt.Errorf("llm_call: persist: %w", err)
		}
	}
	return nil
}

// ai_embed

// executeAIEmbed keeps a row's vector current so manage_vector_search's
// semantic_search can find it. On a data.* event it needs no config at all: the
// row is the event's table/id and its text is the columns create_vector_index
// configured. ref_table / ref_id / text override those. The vector is written to
// the row's _embedding column, the one semantic_search reads.
func (r *Runner) executeAIEmbed(projectDB *sql.DB, event events.Event, config map[string]any) error {
	if r.llmRegistry == nil || r.globalDB == nil {
		return fmt.Errorf("ai_embed: LLM not wired into action runner")
	}
	refTable, _ := config["ref_table"].(string)
	if refTable == "" {
		refTable, _ = event.Payload["table"].(string)
	}
	refID := config["ref_id"]
	if refID == nil || refID == "" {
		refID = event.Payload["id"]
	}
	if refTable == "" || refID == nil || refID == "" {
		return fmt.Errorf("ai_embed: no row to embed: set ref_table and ref_id, or fire it on a data.insert/data.update event")
	}
	table, err := store.SanitizedTableName(refTable)
	if err != nil {
		return fmt.Errorf("ai_embed: %w", err)
	}
	if ok, err := vectorindex.HasColumn(projectDB, table); err != nil {
		return fmt.Errorf("ai_embed: %w", err)
	} else if !ok {
		return fmt.Errorf("ai_embed: %s: %w", table, vectorindex.ErrNoIndex)
	}

	text, _ := config["text"].(string)
	if strings.TrimSpace(text) == "" {
		if text, err = vectorindex.RowText(projectDB, table, refID); err != nil {
			return fmt.Errorf("ai_embed: %w", err)
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil // nothing to embed yet (the indexed columns are empty)
	}

	ctx, cancel := context.WithTimeout(r.lifecycle(), 60*time.Second)
	defer cancel()
	vectors, err := r.embed(ctx, []string{text})
	if err != nil {
		return fmt.Errorf("ai_embed: %w", err)
	}
	if len(vectors) == 0 {
		return fmt.Errorf("ai_embed: provider returned no vectors")
	}
	if err := vectorindex.Store(projectDB, table, refID, vectors[0]); err != nil {
		return fmt.Errorf("ai_embed: store: %w", err)
	}
	return nil
}

// embed runs the configured embeddings provider; a test may substitute embedFn.
func (r *Runner) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if r.embedFn != nil {
		return r.embedFn(ctx, texts)
	}
	return llm.EmbedTexts(ctx, r.globalDB, r.encryptor, r.llmRegistry, texts)
}
