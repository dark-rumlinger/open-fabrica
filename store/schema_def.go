package store

import (
	"encoding/json"
	"sort"
)

// The single reader for of_dynamic_tables.schema_def.
//
// manage_schema records a table as an object keyed by column name whose value is
// EITHER a bare type string ("TEXT") or a constraint object
// ({"type":"TEXT","not_null":true}) - both forms come out of one create_table
// call. Every consumer must handle both, and each reinvented decode got it wrong
// in its own way; parse it here, once.

// SchemaColumn is one column as manage_schema recorded it.
type SchemaColumn struct {
	Name    string
	Type    string
	NotNull bool
	// ServerFills reports that the row gets a value without the caller sending
	// one - a DEFAULT, or a `generate` expression. Such a column is never
	// "required" of a request body or an auto-insert, even when NOT NULL.
	ServerFills bool
}

// ParseSchemaDef decodes a schema_def blob into its columns, name-sorted (map
// iteration is randomized and callers render this into files). A non-object blob
// yields nil; a malformed column is skipped rather than failing the whole table.
func ParseSchemaDef(def string) []SchemaColumn {
	var raw map[string]any
	if json.Unmarshal([]byte(def), &raw) != nil {
		return nil
	}
	cols := make([]SchemaColumn, 0, len(raw))
	for name, v := range raw {
		c := SchemaColumn{Name: name}
		switch spec := v.(type) {
		case string:
			c.Type = spec
		case map[string]any:
			c.Type, _ = spec["type"].(string)
			c.NotNull, _ = spec["not_null"].(bool)
			_, hasDefault := spec["default"]
			_, hasGenerate := spec["generate"]
			c.ServerFills = hasDefault || hasGenerate
		default:
			continue
		}
		if c.Type == "" {
			continue
		}
		cols = append(cols, c)
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	return cols
}

// SchemaDefColumnSet returns just the column names a schema_def declares, for a
// caller that needs membership rather than types.
func SchemaDefColumnSet(def string) map[string]bool {
	cols := ParseSchemaDef(def)
	if len(cols) == 0 {
		return nil
	}
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c.Name] = true
	}
	return set
}
