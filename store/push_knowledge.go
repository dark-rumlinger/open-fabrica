package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"openfabrica/db"
)

// PushEndpointRequiresAuth reports whether a push endpoint exists and requires auth.
func PushEndpointRequiresAuth(pdb *db.ProjectDB, path string) (requiresAuth, exists bool) {
	err := pdb.Reader().QueryRow("SELECT requires_auth FROM of_push_endpoints WHERE path = ?", path).Scan(&requiresAuth)
	if err != nil {
		return false, false
	}
	return requiresAuth, true
}

// UpsertPushSubscription stores or updates a Web Push subscription keyed by endpoint_url.
func UpsertPushSubscription(pdb *db.ProjectDB, path string, userID int, endpointURL, p256dh, authKey string) error {
	var uidArg any
	if userID > 0 {
		uidArg = userID
	}
	_, err := pdb.ExecWrite(`
		INSERT INTO of_push_subscriptions (endpoint_path, user_id, endpoint_url, p256dh, auth_key)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(endpoint_url) DO UPDATE SET
		  p256dh = excluded.p256dh,
		  auth_key = excluded.auth_key,
		  user_id = excluded.user_id`,
		path, uidArg, endpointURL, p256dh, authKey,
	)
	return err
}

// KnowledgeChunk holds a chunk's text and embedding vector.
type KnowledgeChunk struct {
	Text      string
	Embedding []float32
}

// LoadKnowledgeTable returns the chunk table name for baseName, or "" if not found.
func LoadKnowledgeTable(reader *sql.DB, baseName string) string {
	var table string
	if err := reader.QueryRow("SELECT table_name FROM of_knowledge_bases WHERE name = ?", baseName).Scan(&table); err != nil {
		return ""
	}
	return table
}

// LoadKnowledgeChunks reads all embedded chunks from table.
func LoadKnowledgeChunks(reader *sql.DB, table string) ([]KnowledgeChunk, error) {
	rows, err := reader.Query(fmt.Sprintf("SELECT text, _embedding FROM %s WHERE _embedding != '' AND _embedding IS NOT NULL", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []KnowledgeChunk
	for rows.Next() {
		var text, embStr string
		if rows.Scan(&text, &embStr) != nil || embStr == "" {
			continue
		}
		var emb []float32
		if json.Unmarshal([]byte(embStr), &emb) != nil {
			continue
		}
		chunks = append(chunks, KnowledgeChunk{Text: text, Embedding: emb})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}
