package store

import (
	"crypto/sha256"
	"encoding/hex"

	"openfabrica/analysis"
	"openfabrica/internal/dbh"
)

// Incremental maintenance of the symbol index. IndexArtifactSymbols runs from the
// artifact save funnel so symbols re-derive the moment a body changes;
// BackfillStaleSymbols lazily fills the index for older projects. All writes
// happen on the single project writer connection and never inside an open read cursor.

// symbolFingerprint identifies a body for the index: a re-save with an unchanged
// body short-circuits, and the backfill compares this against the stored state.
// Same shape (path + NUL + body, short hex) as the module-parse cache key.
func symbolFingerprint(path, body string) string {
	h := sha256.Sum256([]byte(path + "\x00" + body))
	return hex.EncodeToString(h[:])[:12]
}

// IndexArtifactSymbols re-extracts and stores the symbols/refs for one artifact.
// No-op when the body fingerprint matches; otherwise rows are replaced wholesale
// in one transaction so a reader never sees a half-updated file. Best-effort by
// contract: save-path callers ignore the error so an index failure never blocks a write.
func IndexArtifactSymbols(db dbh.Handle, kind, path, body, contentType string) error {
	fp := symbolFingerprint(path, body)
	var existing string
	_ = db.QueryRow(`SELECT fingerprint FROM of_symbol_index_state WHERE file_kind = ? AND file_path = ?`,
		kind, path).Scan(&existing)
	if existing == fp {
		return nil
	}

	syms, refs := analysis.ExtractSymbols(body, analysis.FileTypeForOutline(path, contentType))

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM of_symbols WHERE file_kind = ? AND file_path = ?`, kind, path); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM of_symbol_refs WHERE ref_file_kind = ? AND ref_file_path = ?`, kind, path); err != nil {
		return err
	}
	for _, s := range syms {
		if s.Name == "" {
			continue
		}
		exported := 0
		if s.Exported {
			exported = 1
		}
		if _, err := tx.Exec(`INSERT INTO of_symbols
			(name, kind, file_kind, file_path, line, signature, exported, fingerprint)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			s.Name, s.Kind, kind, path, s.Line, s.Signature, exported, fp); err != nil {
			return err
		}
	}
	for _, r := range refs {
		if r.Name == "" {
			continue
		}
		target := ""
		if r.Spec != "" {
			if resolved, ok := analysis.ResolveImport(path, r.Spec); ok {
				target = resolved
			}
		}
		if _, err := tx.Exec(`INSERT INTO of_symbol_refs
			(symbol_name, ref_file_kind, ref_file_path, ref_line, ref_kind, target_path, fingerprint)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.Name, kind, path, r.Line, r.RefKind, target, fp); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO of_symbol_index_state (file_kind, file_path, fingerprint, indexed_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(file_kind, file_path) DO UPDATE SET fingerprint = excluded.fingerprint, indexed_at = CURRENT_TIMESTAMP`,
		kind, path, fp); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveArtifactSymbols drops a deleted/renamed artifact's rows from the index.
func RemoveArtifactSymbols(db dbh.Handle, kind, path string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM of_symbols WHERE file_kind = ? AND file_path = ?`, kind, path); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM of_symbol_refs WHERE ref_file_kind = ? AND ref_file_path = ?`, kind, path); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM of_symbol_index_state WHERE file_kind = ? AND file_path = ?`, kind, path); err != nil {
		return err
	}
	return tx.Commit()
}

// BackfillStaleSymbols indexes up to limit stale text artifacts, newest first -
// the self-heal path for a project created before the index existed. Rows are
// fully read into memory before any write so the writer connection is never asked
// to write inside an open read cursor. Returns how many were (re)indexed and how
// many stale files remain (surfaced by lookup tools as a "still warming" hint).
func BackfillStaleSymbols(db dbh.Handle, limit int) (indexed int, remaining int, err error) {
	if limit <= 0 {
		limit = 25
	}

	type candidate struct{ kind, path, body, contentType string }
	rows, err := db.Query(`SELECT kind, path, body, content_type FROM of_files
		WHERE is_deleted = 0 AND storage = 'inline' ORDER BY updated_at DESC`)
	if err != nil {
		return 0, 0, err
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.kind, &c.path, &c.body, &c.contentType); err != nil {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	state := map[string]string{}
	srows, err := db.Query(`SELECT file_kind, file_path, fingerprint FROM of_symbol_index_state`)
	if err == nil {
		for srows.Next() {
			var k, p, fp string
			if srows.Scan(&k, &p, &fp) == nil {
				state[k+"\x00"+p] = fp
			}
		}
		_ = srows.Err() // advisory state; a missed row just means a reindex
		srows.Close()
	}

	for _, c := range cands {
		if !isNewlineNormalizableArtifact(c.kind, c.path) {
			continue
		}
		if state[c.kind+"\x00"+c.path] == symbolFingerprint(c.path, c.body) {
			continue // already indexed and current
		}
		// Stale. Index it while under budget; once the budget is spent, keep
		// scanning only to count how many stale files remain for the next call.
		if indexed >= limit {
			remaining++
			continue
		}
		if err := IndexArtifactSymbols(db, c.kind, c.path, c.body, c.contentType); err != nil {
			remaining++ // could not index this pass; still unindexed
			continue
		}
		indexed++
	}
	return indexed, remaining, nil
}
