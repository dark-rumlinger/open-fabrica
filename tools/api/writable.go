package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// clearWritableSentinel resets writable_columns so every column is writable again.
const clearWritableSentinel = "*"

// privilegedColumnNames are columns a client should not set directly.
var privilegedColumnNames = map[string]bool{
	"role": true, "is_admin": true, "admin": true, "status": true, "paid": true,
	"credits": true, "balance": true, "plan": true, "verified": true,
	"email_verified": true, "is_premium": true,
}

// writableColumnsJSON validates the requested writable columns against the table
// and returns the JSON to store. A nil result with no error means "no restriction".
func writableColumnsJSON(ctx *tools.ToolContext, table string, cols []string) (*string, *tools.Result, error) {
	if len(cols) == 0 || (len(cols) == 1 && cols[0] == clearWritableSentinel) {
		return nil, nil, nil
	}
	known := tableColumnSet(ctx, table)
	for _, c := range cols {
		if err := validateColumnRef("writable_columns", c); err != nil {
			return nil, nil, err
		}
		if known != nil && !known[c] {
			return nil, tools.Fail("writable_columns: %q is not a column of %q", c, table), nil
		}
	}
	b, _ := json.Marshal(cols)
	s := string(b)
	return &s, nil, nil
}

func tableColumnSet(ctx *tools.ToolContext, table string) map[string]bool {
	physical, err := store.SanitizedTableName(table)
	if err != nil {
		return nil
	}
	infos, err := store.GetTableColumnInfo(ctx.DB, physical)
	if err != nil || len(infos) == 0 {
		return nil
	}
	set := make(map[string]bool, len(infos))
	for _, c := range infos {
		set[c.Name] = true
	}
	return set
}

// writableColumnsWarning nudges when a writable endpoint leaves privileged-looking
// columns open to client writes.
func writableColumnsWarning(ctx *tools.ToolContext, table, path string) string {
	var hits []string
	for name := range tableColumnSet(ctx, table) {
		if privilegedColumnNames[strings.ToLower(name)] {
			hits = append(hits, name)
		}
	}
	if len(hits) == 0 {
		return ""
	}
	return fmt.Sprintf("Clients can set every column of %q on POST/PUT, including %s. Restrict it with `manage_api action=update endpoint_path=%s writable_columns=[...]` listing only the fields a client may set, and change privileged fields through a trigger or function.", table, strings.Join(hits, ", "), path)
}

func methodAllowed(methods []string, method string) bool {
	for _, m := range methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}
