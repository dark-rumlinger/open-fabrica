// Package vectorindex is the storage contract for per-row embeddings on a
// project's data tables: which columns a table embeds, and where a row's vector
// lives (its _embedding column). manage_vector_search creates the index and runs
// semantic search over it; the ai_embed trigger action keeps rows embedded as they
// are written. Both go through here, so a vector written by either is one the
// other reads.
package vectorindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"openfabrica/internal/dbh"
	"openfabrica/security"
)

// Column is the per-row vector column create_vector_index adds (JSON float array).
const Column = "_embedding"

// ErrNoIndex means the table has no vector index yet (create_vector_index).
var ErrNoIndex = errors.New("table has no vector index: run manage_vector_search create_vector_index first")

// Columns returns the text columns a table's vector index embeds, as stored by
// create_vector_index in of_dynamic_tables.validation._embedding_config. Nil when
// the table has no index.
func Columns(db dbh.Handle, table string) []string {
	var validation string
	if err := db.QueryRow("SELECT validation FROM of_dynamic_tables WHERE table_name = ?", table).Scan(&validation); err != nil || validation == "" {
		return nil
	}
	var v map[string]any
	if json.Unmarshal([]byte(validation), &v) != nil {
		return nil
	}
	configStr, ok := v["_embedding_config"].(string)
	if !ok {
		return nil
	}
	var config struct {
		Columns []string `json:"columns"`
	}
	if json.Unmarshal([]byte(configStr), &config) != nil {
		return nil
	}
	return config.Columns
}

// SaveColumns records which text columns a table's index embeds (the config
// Columns reads back), in of_dynamic_tables.validation._embedding_config.
func SaveColumns(db dbh.Handle, table string, columns []string) error {
	b, err := json.Marshal(map[string]any{"columns": columns})
	if err != nil {
		return err
	}
	_, err = db.Exec("UPDATE of_dynamic_tables SET validation = json_set(COALESCE(validation, '{}'), '$._embedding_config', ?) WHERE table_name = ?", string(b), table)
	return err
}

// HasColumn reports whether table carries the _embedding column.
func HasColumn(db dbh.Handle, table string) (bool, error) {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, Column).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// RowText is the text the index embeds for one row: its configured columns
// joined by spaces.
func RowText(db dbh.Handle, table string, id any) (string, error) {
	cols := Columns(db, table)
	if len(cols) == 0 {
		return "", ErrNoIndex
	}
	for _, c := range cols {
		// The list comes from stored config but is interpolated below.
		if err := security.ValidateColumnName(c); err != nil {
			return "", fmt.Errorf("invalid embedding column %q in config: %w", c, err)
		}
	}
	var text string
	q := fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", strings.Join(cols, " || ' ' || "), security.QuoteIdent(table)) // #nosec G201 -- validated identifiers
	if err := db.QueryRow(q, id).Scan(&text); err != nil {
		return "", fmt.Errorf("reading row %v of %s: %w", id, table, err)
	}
	return text, nil
}

// Store writes vec as the row's embedding. table must already be validated.
func Store(db dbh.Handle, table string, id any, vec []float32) error {
	b, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", security.QuoteIdent(table), Column), string(b), id) // #nosec G201 -- validated identifier
	return err
}
