package data

import (
	"fmt"
	"strings"

	"openfabrica/events"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

// SQLite supports views, but manage_schema only exposed table DDL. create_view
// /drop_view add a read-only view surface: a named SELECT the model can query
// through manage_sql execute_sql (and describe via describe_table - PRAGMA
// table_info resolves views transparently). The SELECT is validated with the
// same SELECT-only / no-system-table rules execute_sql enforces, so a view can
// never become a write or system-table escape hatch.

// objectKindInMaster returns the sqlite_master "type" for a name ("table",
// "view", "index", ...) or "" when no object by that name exists.
func objectKindInMaster(ctx *tools.ToolContext, name string) string {
	var kind string
	_ = ctx.DB.QueryRow("SELECT type FROM sqlite_master WHERE name = ?", name).Scan(&kind)
	return kind
}

// SchemaCreateView creates (or replaces) a read-only view over project tables.
func SchemaCreateView(ctx *tools.ToolContext, viewName, selectSQL string) (*tools.Result, error) {
	if viewName == "" {
		return tools.Fail("create_view requires a view_name"), nil
	}
	if strings.TrimSpace(selectSQL) == "" {
		return tools.Fail(
			"create_view %q requires a select (the SELECT query the view runs)", viewName), nil
	}

	// Views share SQLite's identifier namespace with tables, so reuse the same
	// name validation (rejects bad identifiers, reserved words, and the exact
	// system-table names) and additionally bar the of_ prefix reserved for
	// internal objects.
	if _, err := store.SanitizedTableName(viewName); err != nil {
		return tools.Fail("%s", err.Error()), nil
	}
	if strings.HasPrefix(strings.ToLower(viewName), "of_") {
		return tools.Fail("cannot use reserved of_ prefix for view: %s", viewName), nil
	}

	// Validate the body with the same contract execute_sql uses: SELECT only,
	// single statement, no system tables.
	trimmedSelect, rejectErr := validateReadOnlySELECT(selectSQL)
	if rejectErr != "" {
		return tools.Fail("%s", "view select: "+rejectErr), nil
	}

	// Never clobber a real table that happens to share the name - dropping it
	// to recreate a view would silently destroy data.
	switch objectKindInMaster(ctx, viewName) {
	case "table":
		return tools.Fail("%q is an existing table, not a view - drop the table or pick another name", viewName), nil
	case "view":
		// Replacing an existing view: SQLite has no CREATE OR REPLACE VIEW, so
		// drop then recreate. This makes create_view idempotent and editable.
		if _, err := ctx.DB.Exec(fmt.Sprintf("DROP VIEW IF EXISTS %s", viewName)); err != nil {
			return nil, fmt.Errorf("replacing view: %w", err)
		}
	}

	if _, err := ctx.DB.Exec(fmt.Sprintf("CREATE VIEW %s AS %s", viewName, trimmedSelect)); err != nil {
		// A bad column/table reference in the SELECT surfaces here - hand it
		// back verbatim so the model can self-correct.
		return tools.Fail("creating view: %v", err), nil
	}

	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventSchemaCreated, ctx.ProjectID, map[string]any{
			"view": viewName,
		}))
	}

	data := map[string]any{
		"view":   viewName,
		"select": trimmedSelect,
	}
	// Read the resolved columns back so the caller sees the real view shape in
	// one shot (PRAGMA table_info works on views).
	if cols, err := store.GetTableColumnInfo(ctx.DB, viewName); err == nil && len(cols) > 0 {
		data["columns"] = cols
	}
	return tools.Ok(data), nil
}

// SchemaDropView removes a view. Non-destructive to data (a view holds none),
// so unlike drop_table it needs no confirm/approval gate.
func SchemaDropView(ctx *tools.ToolContext, viewName string) (*tools.Result, error) {
	if viewName == "" {
		return tools.Fail("view_name is required"), nil
	}
	if err := security.ValidateTableName(viewName); err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	switch objectKindInMaster(ctx, viewName) {
	case "":
		return tools.NotFound("view", viewName), nil
	case "table":
		return tools.Fail("%q is a table, not a view - use drop_table", viewName), nil
	}

	if _, err := ctx.DB.Exec(fmt.Sprintf("DROP VIEW IF EXISTS %s", viewName)); err != nil {
		return nil, fmt.Errorf("dropping view: %w", err)
	}
	return tools.Ok(map[string]any{"dropped_view": viewName}), nil
}
