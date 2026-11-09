package data

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"openfabrica/internal/vectorindex"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

// manage_keyword_search owns the FTS5 keyword path: pure SQLite, no external
// dependencies, always available. Vector / semantic / hybrid actions live
// in manage_vector_search so the embedding-provider gate hides only the
// vector tool from the LLM, not the keyword path.

func searchCreateIndex(ctx *tools.ToolContext, tableName string, columnsRaw []any) (*tools.Result, error) {
	tableName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Validate table exists in dynamic tables.
	var count int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&count); err != nil {
		return tools.Fail("table existence check failed: %v", err), nil
	}
	if count == 0 {
		return tools.Fail("table '%s' not found in dynamic tables", tableName), nil
	}

	// Extract columns.
	if len(columnsRaw) == 0 {
		return tools.Fail("columns must be a non-empty array of column names"), nil
	}
	var columns []string
	for _, c := range columnsRaw {
		col, ok := c.(string)
		if !ok || col == "" {
			return tools.Fail("each column must be a non-empty string"), nil
		}
		if err := security.ValidateColumnName(col); err != nil {
			return tools.Fail("%s", err.Error()), nil
		}
		columns = append(columns, col)
	}

	// Check if FTS index already exists.
	ftsTable := tableName + "_fts"
	var ftsCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", ftsTable).Scan(&ftsCount); err != nil {
		return tools.Fail("FTS index existence check failed: %v", err), nil
	}
	if ftsCount > 0 {
		return tools.Fail("FTS index '%s' already exists, drop it first to recreate", ftsTable), nil
	}

	// Reuse the existing FTS5 creation logic from tables.go.
	if err := CreateFTSIndex(ctx.DB, tableName, columns); err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Populate FTS with existing rows. The index was just created, so a populate
	// failure leaves it empty - surface it rather than reporting success on a
	// half-built index.
	colList := strings.Join(columns, ", ")
	if _, err := ctx.DB.Exec(fmt.Sprintf(
		"INSERT INTO %s(rowid, %s) SELECT id, %s FROM %s",
		ftsTable, colList, colList, tableName,
	)); err != nil {
		return tools.Fail("FTS index created but populating it failed: %v", err), nil
	}

	return tools.Ok(map[string]any{
		"table":   tableName,
		"index":   ftsTable,
		"columns": columns,
	}), nil
}

func searchQuery(ctx *tools.ToolContext, tableName, query string, limit int) (*tools.Result, error) {
	tableName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}

	ftsTable := tableName + "_fts"

	// Verify FTS table exists. A DB fault on the check is reported as such, not
	// misread as "no index" (which would send the caller down the wrong repair path).
	var ftsCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", ftsTable).Scan(&ftsCount); err != nil {
		return tools.Fail("FTS index existence check failed: %v", err), nil
	}
	if ftsCount == 0 {
		return tools.Fail("no full-text search index for table %s. Add one with manage_keyword_search action=create_index table_name=%s columns=[...] (searchable_columns is a manage_schema create_table arg, so it cannot be added to an existing table - do NOT drop and recreate the table for this).", tableName, tableName), nil
	}

	// Query with BM25 ranking.
	sql := fmt.Sprintf(
		`SELECT t.* FROM %s t
		 JOIN %s f ON t.id = f.rowid
		 WHERE %s MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		tableName, ftsTable, ftsTable,
	)
	rows, err := ctx.DB.Query(sql, query, limit)
	if err != nil {
		return tools.Fail("%s", "search failed: "+err.Error()), nil
	}
	defer rows.Close()

	results := ScanRowsMaps(rows)

	// Strip secure columns. An unreadable projection means we can't tell which
	// columns are secret, so refuse rather than hand back rows we couldn't
	// sanitize - these results reach the model's context and the persisted
	// transcript. (An unregistered table reports an empty projection, not an
	// error, so this costs nothing on the happy path.)
	secureCols, refusal := loadSecureColumnsOrFail(ctx, tableName)
	if refusal != nil {
		return refusal, nil
	}
	stripAllSecureCols(results, secureCols)

	return tools.Ok(map[string]any{
		"query":   query,
		"count":   len(results),
		"results": results,
	}), nil
}

func searchDropIndex(ctx *tools.ToolContext, tableName string) (*tools.Result, error) {
	tableName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	ftsTable := tableName + "_fts"
	var exists int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", ftsTable).Scan(&exists)
	if exists == 0 {
		return tools.FailCode(tools.CodeNotFound, "table %q has no keyword index to drop (list_indexes shows which tables have one)", tableName), nil
	}

	// Drop FTS table and triggers. Best-effort: log but keep going so a
	// partial failure still clears what it can.
	drops := []string{
		fmt.Sprintf("DROP TABLE IF EXISTS %s", ftsTable),
		fmt.Sprintf("DROP TRIGGER IF EXISTS %s_ai", tableName),
		fmt.Sprintf("DROP TRIGGER IF EXISTS %s_ad", tableName),
		fmt.Sprintf("DROP TRIGGER IF EXISTS %s_au", tableName),
	}
	for _, stmt := range drops {
		if _, err := ctx.DB.Exec(stmt); err != nil {
			slog.Warn("search drop_index: drop failed", "table", tableName, "stmt", stmt, "error", err)
		}
	}

	return tools.Ok(map[string]any{
		"table":   tableName,
		"dropped": ftsTable,
	}), nil
}

func searchListIndexes(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT name FROM sqlite_master WHERE type='table' AND sql LIKE '%fts5%' ORDER BY name",
	)
	if err != nil {
		return tools.Fail("%s", "listing indexes: "+err.Error()), nil
	}
	defer rows.Close()

	var indexes []map[string]any
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			slog.Warn("search list_indexes: scan failed, skipping row", "error", err)
			continue
		}
		// Derive source table from FTS name.
		sourceTable := strings.TrimSuffix(name, "_fts")
		indexes = append(indexes, map[string]any{
			"index":        name,
			"source_table": sourceTable,
		})
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("%s", "listing indexes: "+err.Error()), nil
	}

	return tools.Ok(indexes), nil
}

// --- Vector / Semantic Search ---

func searchCreateVectorIndex(ctx *tools.ToolContext, tableName string, embeddingColumns []any) (*tools.Result, error) {
	tableName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Get embedding columns.
	if len(embeddingColumns) == 0 {
		return tools.Fail("embedding_columns is required (TEXT columns to embed)"), nil
	}
	var columns []string
	for _, c := range embeddingColumns {
		col, ok := c.(string)
		if !ok || col == "" {
			continue
		}
		if err := security.ValidateColumnName(col); err != nil {
			return tools.Fail("%s", err.Error()), nil
		}
		columns = append(columns, col)
	}

	// Add the per-row vector column when the table does not carry it yet.
	hasCol, err := vectorindex.HasColumn(ctx.DB, tableName)
	if err != nil {
		return tools.Fail("checking for _embedding column: %v", err), nil
	}

	if !hasCol {
		// Add _embedding column.
		_, err := ctx.DB.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN _embedding TEXT DEFAULT ''", tableName))
		if err != nil {
			return tools.Fail("%s", "failed to add _embedding column: "+err.Error()), nil
		}
	}

	if err := vectorindex.SaveColumns(ctx.DB, tableName, columns); err != nil {
		return tools.Fail("storing embedding config: %v", err), nil
	}

	return tools.Ok(map[string]any{
		"table":             tableName,
		"embedding_columns": columns,
		"message":           "Vector index created. Use embed action to generate embeddings for existing rows.",
	}), nil
}

func searchEmbed(ctx *tools.ToolContext, tableName string, rowIDs []any) (*tools.Result, error) {
	tableName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	if ctx.EmbedFunc == nil {
		return tools.Fail("no embedding provider configured, add an OpenAI provider to enable embeddings"), nil
	}

	// Load embedding config.
	columns := vectorindex.Columns(ctx.DB, tableName)
	if len(columns) == 0 {
		return tools.Fail("no embedding columns configured, use create_vector_index first"), nil
	}
	// Defense in depth: the column list comes from stored config, but it is
	// interpolated into the SELECT below, so re-validate before trusting it.
	for _, c := range columns {
		if err := security.ValidateColumnName(c); err != nil {
			return tools.Fail("invalid embedding column %q in config: %v", c, err), nil
		}
	}

	// Build query to get rows.
	colList := strings.Join(columns, " || ' ' || ")
	query := fmt.Sprintf("SELECT id, %s AS _text FROM %s", colList, tableName)
	queryArgs := []any{}

	// Filter by row_ids if provided.
	if len(rowIDs) > 0 {
		placeholders := make([]string, len(rowIDs))
		for i, id := range rowIDs {
			placeholders[i] = "?"
			queryArgs = append(queryArgs, id)
		}
		query += " WHERE id IN (" + strings.Join(placeholders, ",") + ")"
	}

	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return tools.Fail("%s", "query failed: "+err.Error()), nil
	}
	defer rows.Close()

	type rowText struct {
		id   int
		text string
	}
	var items []rowText
	for rows.Next() {
		var rt rowText
		if rows.Scan(&rt.id, &rt.text) == nil {
			items = append(items, rt)
		}
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("%s", "query failed: "+err.Error()), nil
	}

	if len(items) == 0 {
		return tools.Ok(map[string]any{"embedded": 0}), nil
	}

	// Batch embed (max 50 at a time).
	embedded := 0
	batchSize := 50
	for i := 0; i < len(items); i += batchSize {
		end := i + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[i:end]

		texts := make([]string, len(batch))
		for j, item := range batch {
			texts[j] = item.text
		}

		embeddings, err := ctx.EmbedFunc(ctx.Context(), texts)
		if err != nil {
			return tools.Fail("%s", "embedding failed: "+err.Error()), nil
		}

		for j, emb := range embeddings {
			if err := vectorindex.Store(ctx.DB, tableName, batch[j].id, emb); err != nil {
				return tools.Fail("storing embedding for row %d: %v (embedded %d before failure)", batch[j].id, err, embedded), nil
			}
			embedded++
		}
	}

	return tools.Ok(map[string]any{
		"table":    tableName,
		"embedded": embedded,
	}), nil
}

func searchSemantic(ctx *tools.ToolContext, tableName, queryText string, topK int, threshold float64) (*tools.Result, error) {
	tableName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	if ctx.EmbedFunc == nil {
		return tools.Fail("no embedding provider configured"), nil
	}

	if topK <= 0 {
		topK = 10
	}

	// Generate query embedding.
	embeddings, err := ctx.EmbedFunc(ctx.Context(), []string{queryText})
	if err != nil {
		return tools.Fail("%s", "embedding query failed: "+err.Error()), nil
	}
	queryEmb := embeddings[0]

	// Load all rows with embeddings.
	rows, err := ctx.DB.Query(fmt.Sprintf("SELECT * FROM %s WHERE _embedding != '' AND _embedding IS NOT NULL", tableName))
	if err != nil {
		return tools.Fail("%s", "query failed: "+err.Error()), nil
	}
	defer rows.Close()

	allRows := ScanRowsMaps(rows)

	secureCols, refusal := loadSecureColumnsOrFail(ctx, tableName)
	if refusal != nil {
		return refusal, nil
	}

	// Score and rank.
	type scored struct {
		row        map[string]any
		similarity float64
	}
	var results []scored

	for _, row := range allRows {
		embStr, ok := row["_embedding"].(string)
		if !ok || embStr == "" {
			continue
		}
		var rowEmb []float32
		if json.Unmarshal([]byte(embStr), &rowEmb) != nil {
			continue
		}
		sim := cosineSimilarity(queryEmb, rowEmb)
		if sim >= threshold {
			delete(row, "_embedding") // don't return the raw embedding
			stripAllSecureCols([]map[string]any{row}, secureCols)
			results = append(results, scored{row: row, similarity: sim})
		}
	}

	// Sort by similarity descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].similarity > results[j].similarity
	})
	if len(results) > topK {
		results = results[:topK]
	}

	// Format output.
	output := make([]map[string]any, len(results))
	for i, r := range results {
		r.row["_similarity"] = math.Round(r.similarity*1000) / 1000
		output[i] = r.row
	}

	return tools.Ok(map[string]any{
		"query":   queryText,
		"count":   len(output),
		"results": output,
	}), nil
}

func searchHybrid(ctx *tools.ToolContext, tableName, queryText string, topK int) (*tools.Result, error) {
	tableName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	if topK <= 0 {
		topK = 10
	}

	// Run FTS5 search (keyword).
	ftsResults := make(map[int]int) // id -> rank
	ftsTable := tableName + "_fts"
	var ftsCount int
	// Hybrid search degrades to vector-only when FTS is unavailable; a failed
	// existence check is logged rather than fatal so a transient DB error
	// doesn't abort the whole query, but it isn't silently swallowed either.
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", ftsTable).Scan(&ftsCount); err != nil {
		slog.Warn("hybrid search: FTS index existence check failed, continuing vector-only", "table", tableName, "error", err)
	}
	if ftsCount > 0 {
		ftsRows, err := ctx.DB.Query(
			fmt.Sprintf("SELECT t.id FROM %s t JOIN %s f ON t.id = f.rowid WHERE %s MATCH ? ORDER BY rank LIMIT ?",
				tableName, ftsTable, ftsTable),
			queryText, topK*2,
		)
		if err == nil {
			rank := 0
			for ftsRows.Next() {
				var id int
				if ftsRows.Scan(&id) == nil {
					ftsResults[id] = rank
					rank++
				}
			}
			_ = ftsRows.Err() // hybrid degrades to vector-only; tolerated like the Query error
			ftsRows.Close()
		}
	}

	// Run semantic search (vector).
	vecResults := make(map[int]int) // id -> rank
	if ctx.EmbedFunc != nil {
		embeddings, err := ctx.EmbedFunc(ctx.Context(), []string{queryText})
		if err == nil && len(embeddings) > 0 {
			queryEmb := embeddings[0]
			rows, err := ctx.DB.Query(fmt.Sprintf("SELECT id, _embedding FROM %s WHERE _embedding != '' AND _embedding IS NOT NULL", tableName))
			if err == nil {
				type idScore struct {
					id  int
					sim float64
				}
				var scored []idScore
				for rows.Next() {
					var id int
					var embStr string
					if rows.Scan(&id, &embStr) == nil {
						var rowEmb []float32
						if json.Unmarshal([]byte(embStr), &rowEmb) == nil {
							scored = append(scored, idScore{id, cosineSimilarity(queryEmb, rowEmb)})
						}
					}
				}
				_ = rows.Err() // hybrid degrades to keyword-only; tolerated like the Query error
				rows.Close()
				sort.Slice(scored, func(i, j int) bool { return scored[i].sim > scored[j].sim })
				for rank, s := range scored {
					if rank >= topK*2 {
						break
					}
					vecResults[s.id] = rank
				}
			}
		}
	}

	// Reciprocal Rank Fusion (RRF).
	const k = 60
	rrfScores := make(map[int]float64)
	allIDs := make(map[int]bool)
	for id := range ftsResults {
		allIDs[id] = true
	}
	for id := range vecResults {
		allIDs[id] = true
	}
	for id := range allIDs {
		score := 0.0
		if rank, ok := ftsResults[id]; ok {
			score += 1.0 / float64(k+rank)
		}
		if rank, ok := vecResults[id]; ok {
			score += 1.0 / float64(k+rank)
		}
		rrfScores[id] = score
	}

	// Sort by RRF score.
	type idRRF struct {
		id    int
		score float64
	}
	var ranked []idRRF
	for id, score := range rrfScores {
		ranked = append(ranked, idRRF{id, score})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}

	secureCols, refusal := loadSecureColumnsOrFail(ctx, tableName)
	if refusal != nil {
		return refusal, nil
	}

	// Fetch full rows.
	var results []map[string]any
	for _, r := range ranked {
		row, err := ctx.DB.Query(fmt.Sprintf("SELECT * FROM %s WHERE id = ?", tableName), r.id)
		if err != nil {
			continue
		}
		rowMaps := ScanRowsMaps(row)
		row.Close()
		if len(rowMaps) > 0 {
			rowMaps[0]["_rrf_score"] = math.Round(r.score*10000) / 10000
			delete(rowMaps[0], "_embedding")
			stripAllSecureCols(rowMaps[:1], secureCols)
			results = append(results, rowMaps[0])
		}
	}

	return tools.Ok(map[string]any{
		"query":       queryText,
		"count":       len(results),
		"fts_matches": len(ftsResults),
		"vec_matches": len(vecResults),
		"results":     results,
	}), nil
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
