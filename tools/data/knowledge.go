package data

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// manage_knowledge implementation. A knowledge base is a chunk table (kb_<name>)
// with a vector index over its text column. ingest pulls text from a raw string
// or an existing artifact, splits it into overlapping chunks, stores them, and
// embeds them - reusing the vector-search internals (searchCreateVectorIndex /
// searchEmbed / searchSemantic) so there is exactly one embedding/ranking path.

const (
	defaultChunkSize = 1000 // characters per chunk
	defaultOverlap   = 150  // characters of overlap between consecutive chunks
	maxIngestBytes   = 1 << 20

	// Caps for ingest_path so pointing at a huge tree can't run away.
	maxKBFiles      = 1000
	maxKBFileBytes  = 10 << 20  // 10MB per file
	maxKBTotalBytes = 200 << 20 // 200MB total per call
	maxReportItems  = 50        // cap skipped/failed lists in the result
)

// kbTableName maps a base name to its physical chunk table.
func kbTableName(name string) (string, error) {
	return store.SanitizedTableName("kb_" + strings.TrimSpace(name))
}

// resolveKB returns the chunk table name for a base, or an error if unknown.
func resolveKB(ctx *tools.ToolContext, name string) (string, error) {
	var table string
	err := ctx.DB.QueryRow("SELECT table_name FROM of_knowledge_bases WHERE name = ?", strings.TrimSpace(name)).Scan(&table)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no knowledge base named %q; call create_base first", name)
	}
	if err != nil {
		return "", err
	}
	return table, nil
}

func knowledgeCreateBase(ctx *tools.ToolContext, name, description string) (*tools.Result, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return tools.Fail("name is required"), nil
	}
	table, err := kbTableName(name)
	if err != nil {
		return tools.Fail("invalid base name: %s", err.Error()), nil
	}

	// Chunk table: source label, chunk index, and the chunk text we embed.
	if r, err := SchemaCreate(ctx, table, map[string]any{
		"source":      "TEXT",
		"chunk_index": "INTEGER",
		"text":        "TEXT",
	}, []any{"text"}); err != nil {
		return tools.Fail("creating chunk table: %s", err.Error()), nil
	} else if r != nil && !r.Success {
		return r, nil
	}

	if r, err := searchCreateVectorIndex(ctx, table, []any{"text"}); err != nil {
		return tools.Fail("creating vector index: %s", err.Error()), nil
	} else if r != nil && !r.Success {
		return r, nil
	}

	if _, err := ctx.DB.Exec(
		`INSERT INTO of_knowledge_bases (name, table_name, description) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET description = excluded.description`,
		name, table, description); err != nil {
		return tools.Fail("recording knowledge base: %s", err.Error()), nil
	}

	return tools.Ok(map[string]any{
		"base":  name,
		"table": table,
		"next":  "ingest documents, then bind an LLM endpoint with manage_llm_proxy create knowledge_base=" + name,
	}), nil
}

func knowledgeIngest(ctx *tools.ToolContext, base, sourceLabel, text, fromFile string, chunkSize, overlap int) (*tools.Result, error) {
	table, err := resolveKB(ctx, base)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Source: inline text, or a project file (saved by the model, or uploaded in
	// the admin Files tab). A file gets the same per-file cap as ingest_path; the
	// tighter inline cap is for text pasted into the call itself.
	fromFileUsed := false
	if strings.TrimSpace(text) == "" && strings.TrimSpace(fromFile) != "" {
		loaded, err := loadProjectFileText(ctx, fromFile)
		if err != nil {
			return tools.Fail("loading from_file %q: %s", fromFile, err.Error()), nil
		}
		text, fromFileUsed = loaded, true
		if sourceLabel == "" {
			sourceLabel = fromFile
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		if fromFileUsed {
			return tools.Fail("from_file %q has no extractable text (a scanned or image-only PDF has no text layer)", fromFile), nil
		}
		return tools.Fail("provide text to ingest, or from_file pointing at a project file"), nil
	}
	if !fromFileUsed && len(text) > maxIngestBytes {
		return tools.Fail("source exceeds %d byte cap (got %d); ingest in smaller documents", maxIngestBytes, len(text)), nil
	}
	if sourceLabel == "" {
		sourceLabel = "document"
	}
	n, err := ingestDocument(ctx, table, sourceLabel, text, chunkSize, overlap)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}
	return tools.Ok(map[string]any{
		"base":   base,
		"source": sourceLabel,
		"chunks": n,
	}), nil
}

// loadProjectFileText reads a project file and extracts its text with the same
// extractor ingest_path uses, so .pdf and .docx ingest too. It handles both
// storage modes: inline (a file the model saved) and disk (a file uploaded in the
// admin Files tab), reading disk files through store.ReadArtifactBytes, which
// refuses a disk_path outside the project directory.
func loadProjectFileText(ctx *tools.ToolContext, fromFile string) (string, error) {
	path := store.CanonicalFilePath(fromFile)
	var storage, body, diskPath sql.NullString
	err := ctx.DB.QueryRow(
		`SELECT storage, body, disk_path FROM of_files WHERE path = ? AND is_deleted = 0 ORDER BY kind = 'file' DESC LIMIT 1`, path,
	).Scan(&storage, &body, &diskPath)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no project file at that path; list files with manage_files action=list")
	}
	if err != nil {
		return "", err
	}
	data, err := store.ReadArtifactBytes(storage, body, diskPath, ctx.ProjectDir())
	if err != nil {
		return "", err
	}
	if len(data) > maxKBFileBytes {
		return "", fmt.Errorf("file is %d bytes, over the %d byte per-file cap", len(data), maxKBFileBytes)
	}
	text, supported, err := extractText(path, data)
	if !supported {
		return "", fmt.Errorf("binary or non-text file; ingest text files, .pdf, or .docx")
	}
	if err != nil {
		return "", fmt.Errorf("extracting text: %w", err)
	}
	return text, nil
}

// ingestDocument chunks one document's text, stores the chunks in the base
// table, and embeds the newly inserted rows. Returns the chunk count. Shared by
// the inline/from_file ingest and the folder/file ingest_path walker.
func ingestDocument(ctx *tools.ToolContext, table, sourceLabel, text string, chunkSize, overlap int) (int, error) {
	chunkSize, overlap = normalizeChunking(chunkSize, overlap)
	chunks := chunkText(text, chunkSize, overlap)
	if len(chunks) == 0 {
		return 0, nil
	}

	var rowIDs []any
	for i, c := range chunks {
		res, err := ctx.DB.Exec(
			fmt.Sprintf("INSERT INTO %s (source, chunk_index, text) VALUES (?, ?, ?)", table),
			sourceLabel, i, c)
		if err != nil {
			return 0, fmt.Errorf("inserting chunk %d: %w", i, err)
		}
		if id, err := res.LastInsertId(); err == nil {
			rowIDs = append(rowIDs, id)
		}
	}

	if r, err := searchEmbed(ctx, table, rowIDs); err != nil {
		return 0, fmt.Errorf("embedding chunks: %w", err)
	} else if r != nil && !r.Success {
		return 0, fmt.Errorf("%s", r.Error)
	}
	return len(chunks), nil
}

// ingestPathBlocked refuses paths that hold the instance's own secrets: the data
// directory (databases, encryption key, certificates) and config files. Symlinks
// are resolved first so a link cannot point past the check.
func ingestPathBlocked(dataDir, fsPath string) string {
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		return filepath.Clean(p)
	}
	target := resolve(fsPath)
	if dataDir != "" {
		root := resolve(dataDir)
		if target == root || strings.HasPrefix(strings.ToLower(target), strings.ToLower(root)+string(filepath.Separator)) {
			return "the instance data directory cannot be ingested; it holds keys and databases"
		}
	}
	base := strings.ToLower(filepath.Base(target))
	if base == "config.json" || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".pem") ||
		base == ".env" || strings.HasPrefix(base, ".env.") || base == "id_rsa" || base == "id_ed25519" || base == "seed.json" {
		return "config, environment and key files cannot be ingested"
	}
	return ""
}

// knowledgeIngestPath ingests a file or a whole folder from the server's
// filesystem (where OpenFabrica runs). Each supported document is extracted to
// text, chunked, and embedded. Unsupported or undecodable files are reported,
// not fatal. Bounded by maxKBFiles / per-file / total-byte caps.
func knowledgeIngestPath(ctx *tools.ToolContext, base, fsPath string, recursive bool, chunkSize, overlap int) (*tools.Result, error) {
	table, err := resolveKB(ctx, base)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}
	fsPath = strings.TrimSpace(fsPath)
	if fsPath == "" {
		return tools.Fail("path is required (absolute path to a file or folder on the server)"), nil
	}
	if msg := ingestPathBlocked(ctx.DataDirOrDefault(), fsPath); msg != "" {
		return tools.Fail("%s", msg), nil
	}
	// Whatever is ingested becomes searchable by the knowledge base's chat endpoint, so
	// reading arbitrary host paths is the owner's call, not the model's. The question
	// names the exact path; the build resumes with the answer.
	if abs, aerr := filepath.Abs(fsPath); aerr == nil {
		key := "ingest_path:" + strings.ToLower(abs)
		if !tools.CheckApproval(ctx.DB, key) {
			return tools.RequestApproval(ctx, key,
				fmt.Sprintf("The AI wants to read %q from the server's filesystem and add its text to the knowledge base %q, where the project's chat endpoint can quote it to visitors. Approve?", abs, base),
				fmt.Sprintf("Ingesting %q into knowledge base %q.", abs, base),
			), nil
		}
	}
	info, err := os.Stat(fsPath)
	if err != nil {
		return tools.Fail("cannot access path %q: %s", fsPath, err.Error()), nil
	}

	// Collect candidate files.
	var files []string
	dirRoot := fsPath
	if info.IsDir() {
		if recursive {
			// Walk is best-effort: unreadable entries are skipped, never fatal.
			_ = filepath.WalkDir(fsPath, func(p string, d os.DirEntry, werr error) error {
				if werr != nil || d.IsDir() {
					return nil
				}
				if !isBinaryExt(p) {
					files = append(files, p)
				}
				return nil
			})
		} else {
			entries, _ := os.ReadDir(fsPath)
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				p := filepath.Join(fsPath, e.Name())
				if !isBinaryExt(p) {
					files = append(files, p)
				}
			}
		}
	} else {
		files = []string{fsPath}
		dirRoot = filepath.Dir(fsPath)
	}

	if len(files) == 0 {
		return tools.Fail("no ingestible files found under %q. Ingests any UTF-8 text/code file plus .pdf and .docx; binary files (images, archives, media) are skipped", fsPath), nil
	}
	truncatedFileList := false
	if len(files) > maxKBFiles {
		files = files[:maxKBFiles]
		truncatedFileList = true
	}

	var (
		ingestedFiles, totalChunks int
		totalBytes                 int64
		skipped, failed            []map[string]any
		hitTotalCap                bool
	)
	addReport := func(list *[]map[string]any, file, reason string) {
		if len(*list) < maxReportItems {
			*list = append(*list, map[string]any{"file": file, "reason": reason})
		}
	}

	for _, p := range files {
		rel, relErr := filepath.Rel(dirRoot, p)
		if relErr != nil {
			rel = filepath.Base(p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			addReport(&failed, rel, "read error: "+err.Error())
			continue
		}
		if len(data) > maxKBFileBytes {
			addReport(&skipped, rel, "exceeds per-file size cap")
			continue
		}
		if totalBytes+int64(len(data)) > maxKBTotalBytes {
			hitTotalCap = true
			break
		}
		totalBytes += int64(len(data))

		text, supported, exErr := extractText(p, data)
		if !supported {
			addReport(&skipped, rel, "binary or non-text file")
			continue
		}
		if exErr != nil {
			addReport(&failed, rel, "extract error: "+exErr.Error())
			continue
		}
		if strings.TrimSpace(text) == "" {
			addReport(&skipped, rel, "no extractable text (scanned/image PDF has no text layer)")
			continue
		}
		n, err := ingestDocument(ctx, table, rel, text, chunkSize, overlap)
		if err != nil {
			addReport(&failed, rel, err.Error())
			continue
		}
		ingestedFiles++
		totalChunks += n
	}

	out := map[string]any{
		"base":           base,
		"ingested_files": ingestedFiles,
		"total_chunks":   totalChunks,
		"skipped_count":  len(skipped),
		"failed_count":   len(failed),
	}
	if len(skipped) > 0 {
		out["skipped"] = skipped
	}
	if len(failed) > 0 {
		out["failed"] = failed
	}
	if truncatedFileList {
		out["note"] = fmt.Sprintf("more than %d files found; only the first %d were processed", maxKBFiles, maxKBFiles)
	}
	if hitTotalCap {
		out["note"] = fmt.Sprintf("stopped after reaching the %dMB total-bytes cap", maxKBTotalBytes>>20)
	}
	if ingestedFiles == 0 {
		return &tools.Result{Success: false, Error: "no documents were ingested (all skipped or failed)", Data: out}, nil
	}
	return tools.Ok(out), nil
}

func knowledgeRetrieve(ctx *tools.ToolContext, base, query string, topK int) (*tools.Result, error) {
	table, err := resolveKB(ctx, base)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}
	if strings.TrimSpace(query) == "" {
		return tools.Fail("query is required"), nil
	}
	if topK <= 0 {
		topK = 5
	}
	// Threshold 0 so retrieval always returns the best matches; the caller ranks.
	return searchSemantic(ctx, table, query, topK, 0.0)
}

func knowledgeListBases(ctx *tools.ToolContext) (*tools.Result, error) {
	type baseEntry struct {
		name  string
		table string
		desc  string
	}

	rows, err := ctx.DB.Query("SELECT name, table_name, description FROM of_knowledge_bases ORDER BY name")
	if err != nil {
		return tools.Fail("listing knowledge bases: %s", err.Error()), nil
	}
	var collected []baseEntry
	for rows.Next() {
		var b baseEntry
		if rows.Scan(&b.name, &b.table, &b.desc) != nil {
			continue
		}
		collected = append(collected, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return tools.Fail("listing knowledge bases: %s", err.Error()), nil
	}

	// Now that the cursor is closed, count each base's chunks. ctx.DB has
	// MaxOpenConns=1, so the count query would deadlock if we ran it inside
	// the rows.Next() loop above.
	out := []map[string]any{}
	for _, b := range collected {
		var chunks int
		// Table name comes from our own registry and was sanitized at creation.
		_ = ctx.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", b.table)).Scan(&chunks) // best-effort count; 0 on failure
		out = append(out, map[string]any{
			"base": b.name, "description": b.desc, "chunks": chunks,
		})
	}
	return tools.Ok(map[string]any{"knowledge_bases": out}), nil
}

func knowledgeDeleteBase(ctx *tools.ToolContext, base string) (*tools.Result, error) {
	table, err := resolveKB(ctx, base)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}
	name := strings.TrimSpace(base)
	// An LLM endpoint bound to this base would answer without its documents, or
	// fail, with nothing saying why. Refuse until it is pointed elsewhere.
	if rows, qerr := ctx.DB.Query("SELECT path FROM of_llm_endpoints WHERE knowledge_base = ?", name); qerr == nil {
		var bound []string
		for rows.Next() {
			var path string
			if rows.Scan(&path) == nil {
				bound = append(bound, path)
			}
		}
		iterErr := rows.Err()
		rows.Close()
		// A half-read list could hide a bound endpoint and let the base be dropped.
		if iterErr != nil {
			return tools.Fail("could not check which LLM endpoints use knowledge base %q: %v", name, iterErr), nil
		}
		if len(bound) > 0 {
			return tools.Fail("knowledge base %q is used by the LLM endpoint(s) %s; rebind or remove them first (manage_llm_proxy update), then delete the base", name, strings.Join(bound, ", ")), nil
		}
	}
	if _, err := ctx.DB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", table)); err != nil {
		return tools.Fail("dropping the knowledge base table: %v", err), nil
	}
	if _, err := ctx.DB.Exec("DELETE FROM of_dynamic_tables WHERE table_name = ?", table); err != nil {
		return tools.Fail("removing the knowledge base from the table registry: %v", err), nil
	}
	if _, err := ctx.DB.Exec("DELETE FROM of_knowledge_bases WHERE name = ?", name); err != nil {
		return tools.Fail("removing the knowledge base record: %v", err), nil
	}
	return tools.Ok(map[string]any{"deleted": base}), nil
}

// normalizeChunking applies the defaults and keeps the overlap well under the chunk
// size, so consecutive windows always advance. An overlap at or past half the size
// would make progress per chunk shrink toward nothing.
func normalizeChunking(size, overlap int) (int, int) {
	if size <= 0 {
		size = defaultChunkSize
	}
	if overlap < 0 {
		overlap = defaultOverlap
	}
	if overlap > size/2 {
		overlap = size / 4
	}
	return size, overlap
}

// chunkText splits text into chunks of about size characters, each overlapping
// the previous by overlap characters, breaking on whitespace so words stay
// whole. Overlap preserves context across chunk boundaries for retrieval.
func chunkText(text string, size, overlap int) []string {
	size, overlap = normalizeChunking(size, overlap)
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) == 0 {
		return nil
	}
	if len(runes) <= size {
		return []string{string(runes)}
	}
	var chunks []string
	start := 0
	for start < len(runes) {
		end := start + size
		if end >= len(runes) {
			chunks = append(chunks, strings.TrimSpace(string(runes[start:])))
			break
		}
		// Back up to the last space within the window so we don't split a word.
		cut := end
		for cut > start && runes[cut] != ' ' {
			cut--
		}
		if cut == start {
			cut = end // no space found; hard split
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[start:cut])))
		// Always move forward: with the overlap back-step alone, a window whose last
		// space sits within overlap of its start would land on or before start and
		// loop forever, growing the chunk list without bound.
		next := cut - overlap
		if next <= start {
			next = start + 1
		}
		start = next
		// Skip a leading space introduced by the overlap back-step.
		for start < len(runes) && runes[start] == ' ' {
			start++
		}
	}
	return chunks
}
