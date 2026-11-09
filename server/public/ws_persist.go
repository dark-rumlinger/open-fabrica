package public

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"openfabrica/db"
	"openfabrica/security"
)

// Durable persistence for relay endpoints (write_to_table) plus the two cleanup
// sweeps: occupancy-based (ephemeral rooms) and age-based (disappearing messages).

// wsWriteToTable inserts a WebSocket message into the configured table,
// mapping JSON fields to table columns using the cached schema. When the
// connection is user-scoped (JWT-authed) and the target table has a
// `user_id` column, that column is auto-stamped from the JWT claims -
// the client cannot spoof it via the message payload.
func wsWriteToTable(projectDB *db.ProjectDB, ws *wsEndpointConfig, msgMap map[string]any, userID, tenantID int, roomName string) {
	// Work on a copy so the caller's frame is never mutated. On an e2ee endpoint
	// persist ONLY the ciphertext envelope - never arbitrary client fields, which
	// could smuggle cleartext into the durable row. The room column is always
	// stamped from the server-tracked room, so a client cannot persist a row tagged
	// a room it never joined (room-gated endpoints disable in-band switching, yet
	// the client still controls the frame's room field).
	work := make(map[string]any, len(msgMap)+1)
	if ws.E2EE {
		for _, k := range e2eeEnvelopeKeys {
			if v, ok := msgMap[k]; ok {
				work[k] = v
			}
		}
	} else {
		for k, v := range msgMap {
			work[k] = v
		}
	}
	if ws.RoomColumn != "" && roomName != "" {
		work[ws.RoomColumn] = roomName
	}
	msgMap = work

	if len(ws.tableColumns) == 0 {
		// Target table isn't in of_dynamic_tables, write the raw JSON to a `data` column.
		raw, _ := json.Marshal(msgMap)
		if _, err := projectDB.ExecWrite(
			fmt.Sprintf("INSERT INTO %s (data, created_at) VALUES (?, datetime('now'))", security.QuoteIdent(ws.WriteToTable)),
			string(raw),
		); err != nil {
			slog.Warn("ws write_to_table insert failed", "table", ws.WriteToTable, "error", err)
		}
		return
	}

	// Map incoming JSON fields to table columns.
	var cols []string
	var placeholders []string
	var vals []any
	_, hasUserIDCol := ws.tableColumns["user_id"]
	hasTenantCol := false
	if ws.TenantColumn != "" {
		_, hasTenantCol = ws.tableColumns[ws.TenantColumn]
	}
	for colName := range ws.tableColumns {
		if colName == "id" || colName == "created_at" {
			continue
		}
		// Server-stamped user_id and tenant_column win over any client-supplied
		// value; never trust the wire payload on these columns.
		if colName == "user_id" {
			continue
		}
		if hasTenantCol && colName == ws.TenantColumn {
			continue
		}
		val, exists := msgMap[colName]
		if !exists {
			continue
		}
		cols = append(cols, colName)
		placeholders = append(placeholders, "?")
		vals = append(vals, val)
	}
	if hasUserIDCol && userID > 0 {
		cols = append(cols, "user_id")
		placeholders = append(placeholders, "?")
		vals = append(vals, userID)
	}
	if hasTenantCol && tenantID > 0 {
		cols = append(cols, ws.TenantColumn)
		placeholders = append(placeholders, "?")
		vals = append(vals, tenantID)
	}

	if len(cols) == 0 {
		return // no matching fields
	}

	// A relay endpoint carries heterogeneous frames (a break/place edit fills the
	// durable row; a chat/move/presence frame does not). Attempting the insert for a
	// frame that omits a required NOT NULL column just throws a constraint error and
	// spams a WARN per frame, so skip it quietly here instead. The genuine edits (all
	// required columns present) still persist. cols already holds the message-matched
	// columns plus any server-stamped user_id/tenant.
	if len(ws.requiredColumns) > 0 {
		provided := make(map[string]bool, len(cols))
		for _, c := range cols {
			provided[c] = true
		}
		for req := range ws.requiredColumns {
			if !provided[req] {
				slog.Debug("ws write_to_table: skipping frame missing required column",
					"table", ws.WriteToTable, "missing", req)
				return
			}
		}
	}

	// Always include created_at.
	cols = append(cols, "created_at")
	placeholders = append(placeholders, "datetime('now')")

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		security.QuoteIdent(ws.WriteToTable),
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "),
	)
	if _, err := projectDB.ExecWrite(query, vals...); err != nil {
		slog.Warn("ws write_to_table insert failed", "table", ws.WriteToTable, "error", err)
	}
}

// pruneEphemeralRoom deletes an ephemeral endpoint's persisted rows for a room
// that just emptied. It is the server-side answer to "nothing stored after the
// session ends": the relay owns cleanup, so a generated app never reaches for a
// client-driven delete (which it cannot do safely from beforeunload anyway).
// No-op unless the endpoint is ephemeral with a write_to_table AND a room_column
// to scope the delete; a tenant_column further constrains it so one tenant's
// teardown never touches another tenant's rows.
func pruneEphemeralRoom(projectDB *db.ProjectDB, ws *wsEndpointConfig, roomName string, tenantID int) {
	if ws == nil || !ws.Ephemeral {
		return
	}
	if ws.WriteToTable == "" || ws.RoomColumn == "" || roomName == "" {
		return
	}
	if !validTableName.MatchString(ws.WriteToTable) || !validTableName.MatchString(ws.RoomColumn) {
		return
	}
	// Only scope by a room column the table actually has; without it we'd risk a
	// table-wide delete or a malformed query, so bail rather than over-delete.
	if len(ws.tableColumns) > 0 {
		if _, ok := ws.tableColumns[ws.RoomColumn]; !ok {
			return
		}
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = ?",
		security.QuoteIdent(ws.WriteToTable), security.QuoteIdent(ws.RoomColumn))
	args := []any{roomName}
	if ws.TenantColumn != "" && tenantID > 0 && validTableName.MatchString(ws.TenantColumn) {
		if _, ok := ws.tableColumns[ws.TenantColumn]; ok {
			query += fmt.Sprintf(" AND %s = ?", security.QuoteIdent(ws.TenantColumn))
			args = append(args, tenantID)
		}
	}
	if _, err := projectDB.ExecWrite(query, args...); err != nil {
		slog.Warn("ws ephemeral room prune failed", "table", ws.WriteToTable, "room", roomName, "error", err)
	}
}

// pruneExpiredMessages sweeps an endpoint's persisted messages older than
// MessageTTLSeconds from its write_to_table ("disappearing messages"). Unlike
// pruneEphemeralRoom (occupancy-based, one room), this is age-based and table-wide,
// so history self-expires even in rooms that never empty. Called lazily on relay
// activity. Best-effort; no-op unless the endpoint has a positive TTL and a valid
// write_to_table carrying created_at.
func pruneExpiredMessages(projectDB *db.ProjectDB, ws *wsEndpointConfig, tenantID int) {
	if ws == nil || ws.MessageTTLSeconds <= 0 || ws.WriteToTable == "" {
		return
	}
	if !validTableName.MatchString(ws.WriteToTable) {
		return
	}
	if len(ws.tableColumns) > 0 {
		if _, ok := ws.tableColumns["created_at"]; !ok {
			return
		}
	}
	// MessageTTLSeconds is a validated int (never user text), so embedding it in
	// the datetime modifier is injection-safe.
	query := fmt.Sprintf("DELETE FROM %s WHERE created_at < datetime('now', '-%d seconds')",
		security.QuoteIdent(ws.WriteToTable), ws.MessageTTLSeconds)
	var args []any
	// Scope to the tenant when the endpoint is tenanted so one tenant's sweep
	// never reaches another's rows (age applies per tenant either way).
	if ws.TenantColumn != "" && tenantID > 0 && validTableName.MatchString(ws.TenantColumn) {
		if _, ok := ws.tableColumns[ws.TenantColumn]; ok {
			query += fmt.Sprintf(" AND %s = ?", security.QuoteIdent(ws.TenantColumn))
			args = append(args, tenantID)
		}
	}
	if _, err := projectDB.ExecWrite(query, args...); err != nil {
		slog.Warn("ws expired-message prune failed", "table", ws.WriteToTable, "error", err)
	}
}
