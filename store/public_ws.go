package store

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/db"
	"openfabrica/security"
)

// StreamEndpointRow holds raw columns from of_stream_endpoints.
type StreamEndpointRow struct {
	Path           string
	EventTypesJSON string
	TableName      string
	RequiresAuth   bool
}

// LoadStreamEndpoint loads a stream endpoint configuration by path.
func LoadStreamEndpoint(reader *sql.DB, path string) (*StreamEndpointRow, error) {
	var r StreamEndpointRow
	var tableName sql.NullString
	err := reader.QueryRow(
		"SELECT path, event_types, table_name, requires_auth FROM of_stream_endpoints WHERE path = ?",
		path,
	).Scan(&r.Path, &r.EventTypesJSON, &tableName, &r.RequiresAuth)
	if err != nil {
		return nil, err
	}
	r.TableName = tableName.String
	return &r, nil
}

// HasAnyScopedAPIEndpoint reports whether any of_api_endpoints row defines owner or tenant scoping.
func HasAnyScopedAPIEndpoint(reader *sql.DB) (bool, error) {
	var n int
	err := reader.QueryRow(
		`SELECT COUNT(*) FROM of_api_endpoints WHERE COALESCE(owner_column,'') != '' OR COALESCE(tenant_column,'') != ''`,
	).Scan(&n)
	return n > 0, err
}

// LoadDynamicTableSchemaDef loads schema_def for tableName from of_dynamic_tables.
func LoadDynamicTableSchemaDef(reader *sql.DB, tableName string) (string, error) {
	var schemaDef sql.NullString
	err := reader.QueryRow("SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&schemaDef)
	return schemaDef.String, err
}

// GetWSRoomToken looks up an existing room access token.
func GetWSRoomToken(reader *sql.DB, endpointPath string, tenantID int, room string) (string, error) {
	var stored string
	err := reader.QueryRow(
		"SELECT token FROM of_ws_room_tokens WHERE endpoint_path = ? AND tenant_id = ? AND room = ?",
		endpointPath, tenantID, room,
	).Scan(&stored)
	return stored, err
}

// ClaimWSRoomToken inserts minted if unclaimed and returns the winning token.
func ClaimWSRoomToken(pdb *db.ProjectDB, endpointPath string, tenantID int, room, minted string) (string, error) {
	if _, err := pdb.ExecWrite(
		"INSERT OR IGNORE INTO of_ws_room_tokens (endpoint_path, tenant_id, room, token) VALUES (?, ?, ?, ?)",
		endpointPath, tenantID, room, minted,
	); err != nil {
		return "", err
	}
	var final string
	if err := pdb.QueryRow(
		"SELECT token FROM of_ws_room_tokens WHERE endpoint_path = ? AND tenant_id = ? AND room = ?",
		endpointPath, tenantID, room,
	).Scan(&final); err != nil {
		return "", err
	}
	return final, nil
}

// DeleteWSRoomToken removes a room access token when the room empties.
func DeleteWSRoomToken(pdb *db.ProjectDB, endpointPath string, tenantID int, room string) error {
	_, err := pdb.ExecWrite(
		"DELETE FROM of_ws_room_tokens WHERE endpoint_path = ? AND tenant_id = ? AND room = ?",
		endpointPath, tenantID, room,
	)
	return err
}

// InsertWSRawJSON inserts a raw JSON message into a fallback WebSocket table's `data` column.
func InsertWSRawJSON(pdb *db.ProjectDB, table, rawJSON string) error {
	_, err := pdb.ExecWrite(
		fmt.Sprintf("INSERT INTO %s (data, created_at) VALUES (?, datetime('now'))", security.QuoteIdent(table)),
		rawJSON,
	)
	return err
}

// InsertWSRow inserts a mapped WebSocket message row with created_at = datetime('now').
func InsertWSRow(pdb *db.ProjectDB, table string, cols []string, vals []any) error {
	quoted := make([]string, len(cols)+1)
	placeholders := make([]string, len(cols)+1)
	for i, c := range cols {
		quoted[i] = security.QuoteIdent(c)
		placeholders[i] = "?"
	}
	quoted[len(cols)] = "created_at"
	placeholders[len(cols)] = "datetime('now')"

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		security.QuoteIdent(table),
		strings.Join(quoted, ", "),
		strings.Join(placeholders, ", "),
	)
	_, err := pdb.ExecWrite(query, vals...)
	return err
}

// DeleteWSEphemeralRoom deletes persisted rows for an emptied ephemeral room,
// optionally scoped to tenantID when tenantCol is non-empty.
func DeleteWSEphemeralRoom(pdb *db.ProjectDB, table, roomCol, roomName, tenantCol string, tenantID int) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = ?",
		security.QuoteIdent(table), security.QuoteIdent(roomCol))
	args := []any{roomName}
	if tenantCol != "" && tenantID > 0 {
		query += fmt.Sprintf(" AND %s = ?", security.QuoteIdent(tenantCol))
		args = append(args, tenantID)
	}
	_, err := pdb.ExecWrite(query, args...)
	return err
}

// DeleteWSExpiredMessages deletes rows older than ttlSeconds, optionally scoped
// to tenantID when tenantCol is non-empty.
func DeleteWSExpiredMessages(pdb *db.ProjectDB, table string, ttlSeconds int, tenantCol string, tenantID int) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE created_at < datetime('now', '-%d seconds')",
		security.QuoteIdent(table), ttlSeconds)
	var args []any
	if tenantCol != "" && tenantID > 0 {
		query += fmt.Sprintf(" AND %s = ?", security.QuoteIdent(tenantCol))
		args = append(args, tenantID)
	}
	_, err := pdb.ExecWrite(query, args...)
	return err
}

// LoadWSRoomState loads persisted state and revision for a room actor.
func LoadWSRoomState(reader *sql.DB, endpointPath string, tenantID int, room string) (state string, rev int, err error) {
	err = reader.QueryRow(
		`SELECT state, rev FROM of_ws_room_state WHERE endpoint_path = ? AND tenant_id = ? AND room = ?`,
		endpointPath, tenantID, room,
	).Scan(&state, &rev)
	return state, rev, err
}

// SaveWSRoomState upserts persisted state and revision for a room actor.
func SaveWSRoomState(writer *sql.DB, endpointPath string, tenantID int, room, state string, rev int) error {
	_, err := writer.Exec(
		`INSERT INTO of_ws_room_state (endpoint_path, tenant_id, room, state, rev, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(endpoint_path, tenant_id, room) DO UPDATE SET
		   state = excluded.state, rev = excluded.rev, updated_at = CURRENT_TIMESTAMP`,
		endpointPath, tenantID, room, state, rev,
	)
	return err
}

// DeleteWSRoomState removes persisted state for a room actor (best-effort).
func DeleteWSRoomState(writer *sql.DB, endpointPath string, tenantID int, room string) {
	bestEffortExec(
		writer,
		"delete ws room state",
		`DELETE FROM of_ws_room_state WHERE endpoint_path = ? AND tenant_id = ? AND room = ?`,
		endpointPath, tenantID, room,
	)
}

// LoadSingleRowByKey selects the named columns for the first row matching keyCol = keyVal.
func LoadSingleRowByKey(reader *sql.DB, table, keyCol string, keyVal any, cols []string) (map[string]any, error) {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = security.QuoteIdent(c)
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ? LIMIT 1",
		strings.Join(quoted, ", "), security.QuoteIdent(table), security.QuoteIdent(keyCol))

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := reader.QueryRow(query, keyVal).Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		if b, ok := vals[i].([]byte); ok {
			out[c] = string(b)
		} else {
			out[c] = vals[i]
		}
	}
	return out, nil
}

// LoadTableRows reads up to limit rows from table, selecting cols (or * when cols is empty).
func LoadTableRows(reader *sql.DB, table string, cols []string, limit int) ([]map[string]any, error) {
	colSpec := "*"
	if len(cols) > 0 {
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = security.QuoteIdent(c)
		}
		colSpec = strings.Join(quoted, ", ")
	}
	query := fmt.Sprintf("SELECT %s FROM %s LIMIT %d", colSpec, security.QuoteIdent(table), limit)
	rows, err := reader.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	colNames, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		row := make(map[string]any, len(colNames))
		for i, c := range colNames {
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
