// Package wsendpoint is the single source of truth for the of_ws_endpoints row
// schema: the column list and its COALESCE defaults live in exactly one place,
// so adding a Row field + selectExpr entry updates every reader. Depends only on
// database/sql (+ encoding/json, time) so no import cycles form through it.
package wsendpoint

import (
	"encoding/json"
	"time"

	"openfabrica/internal/dbh"
)

// Row is a full of_ws_endpoints row. String/int fields carry the COALESCE default
// (never NULL), so callers scan into plain types.
type Row struct {
	ID                int
	Path              string
	WriteToTable      string
	RoomColumn        string
	RequiresAuth      bool
	TickHz            int
	RateLimit         int
	MaxMsgSize        int
	TenantColumn      string
	E2EE              bool
	Ephemeral         bool
	RoomGated         bool
	Matchmaking       bool
	GroupSize         int
	MaxClients        int
	Authoritative     bool
	Reducer           string
	MessageTTLSeconds int
	SeedTable         string
	SeedKeyColumn     string
	SeedColumns       string // raw JSON array as stored
	MatchBy           string
	RatingBand        int
	BandWidenPerSec   float64
	MaxWaitSecs       int
	SharedTable       string
	SharedColumns     string // raw JSON array as stored
	// Model is the realtime tool that created the endpoint ("" on legacy rows);
	// EffectiveModel infers one for those.
	Model     string
	CreatedAt time.Time
}

// selectExpr is the ordered SELECT list (with COALESCE defaults) shared by Load
// and LoadAll. The scan order in scanRow MUST match this order exactly.
const selectExpr = `id, path, COALESCE(write_to_table,''), COALESCE(room_column,''), requires_auth, ` +
	`COALESCE(tick_hz,0), COALESCE(rate_limit,0), COALESCE(max_msg_size,0), COALESCE(tenant_column,''), ` +
	`COALESCE(e2ee,0), COALESCE(ephemeral,0), COALESCE(room_gated,0), COALESCE(matchmaking,0), ` +
	`COALESCE(group_size,2), COALESCE(max_clients,0), COALESCE(authoritative,0), COALESCE(reducer,''), ` +
	`COALESCE(message_ttl_seconds,0), COALESCE(seed_table,''), COALESCE(seed_key_column,''), ` +
	`COALESCE(seed_columns,''), COALESCE(match_by,''), COALESCE(rating_band,200), ` +
	`COALESCE(band_widen_per_sec,0), COALESCE(max_wait_secs,0), COALESCE(shared_table,''), ` +
	`COALESCE(shared_columns,''), COALESCE(model,''), created_at`

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanRow(s scanner) (*Row, error) {
	var r Row
	if err := s.Scan(
		&r.ID, &r.Path, &r.WriteToTable, &r.RoomColumn, &r.RequiresAuth,
		&r.TickHz, &r.RateLimit, &r.MaxMsgSize, &r.TenantColumn,
		&r.E2EE, &r.Ephemeral, &r.RoomGated, &r.Matchmaking,
		&r.GroupSize, &r.MaxClients, &r.Authoritative, &r.Reducer,
		&r.MessageTTLSeconds, &r.SeedTable, &r.SeedKeyColumn,
		&r.SeedColumns, &r.MatchBy, &r.RatingBand,
		&r.BandWidenPerSec, &r.MaxWaitSecs, &r.SharedTable,
		&r.SharedColumns, &r.Model, &r.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &r, nil
}

// Load reads one endpoint row by path. Returns sql.ErrNoRows when absent.
func Load(db dbh.Handle, path string) (*Row, error) {
	return scanRow(db.QueryRow("SELECT "+selectExpr+" FROM of_ws_endpoints WHERE path = ?", path))
}

// LoadAll reads every endpoint row, ordered by path.
func LoadAll(db dbh.Handle) ([]*Row, error) {
	rows, err := db.Query("SELECT " + selectExpr + " FROM of_ws_endpoints ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeedColumnList parses the stored seed_columns JSON array (nil when empty).
func (r *Row) SeedColumnList() []string { return parseJSONArray(r.SeedColumns) }

// SharedColumnList parses the stored shared_columns JSON array (nil when empty).
func (r *Row) SharedColumnList() []string { return parseJSONArray(r.SharedColumns) }

// Realtime models, one per model tool (manage_<model>).
const (
	ModelRelay         = "relay"
	ModelTick          = "tick"
	ModelAuthoritative = "authoritative"
	ModelMatchmaking   = "matchmaking"
	ModelSignaling     = "signaling"
)

// EffectiveModel is the stored model, or - for a row created before the column
// existed - the one its flags imply. Signaling is indistinguishable from relay on
// such a row, so it infers relay.
func (r *Row) EffectiveModel() string {
	if r.Model != "" {
		return r.Model
	}
	switch {
	case r.Authoritative || r.Reducer != "":
		return ModelAuthoritative
	case r.Matchmaking:
		return ModelMatchmaking
	case r.TickHz > 0:
		return ModelTick
	}
	return ModelRelay
}

// HasReducer reports whether an authoritative reducer is stored.
func (r *Row) HasReducer() bool { return r.Reducer != "" }

func parseJSONArray(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}
