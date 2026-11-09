package data

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"openfabrica/events"
	"openfabrica/internal/dbh"
	"openfabrica/internal/endpoints"
	"openfabrica/internal/sqlguard"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
	"openfabrica/tools/api"
)

// allowedColumnTypes are the SQLite types permitted in dynamic tables. PASSWORD and
// ENCRYPTED are stored as TEXT but handled specially on insert/query. Package-local
// because only the DDL handlers below consult it; the cross-family path uses
// store.ValidTableName / store.ValidColumnName and store.SecureColumnKinds.
var allowedColumnTypes = map[string]string{
	"TEXT":      "TEXT",
	"INTEGER":   "INTEGER",
	"REAL":      "REAL",
	"BLOB":      "BLOB",
	"BOOLEAN":   "BOOLEAN",
	"PASSWORD":  "TEXT", // stored as bcrypt hash
	"ENCRYPTED": "TEXT", // stored as AES-encrypted base64
}

// schemaColumnDef holds a parsed column definition with optional constraints.
type schemaColumnDef struct {
	Name        string
	Type        string // logical type (TEXT, INTEGER, REAL, BOOLEAN, PASSWORD, ENCRYPTED)
	SQLType     string // mapped SQLite type
	NotNull     bool
	Unique      bool
	Default     string // user-supplied default, quoted as a LITERAL (e.g. "'draft'" or "0")
	DefaultExpr string // safe generated DEFAULT expression, emitted VERBATIM (from `generate`)
	Check       string // raw SQL check expression (e.g. "> 0")
	References  string // foreign key reference (e.g. "users(id)"), generates REFERENCES clause
	IsSecure    bool
	Kind        string // "hash" or "encrypt" for secure columns
}

// validFKReference is a small adapter over the canonical security.ValidateFKReference
// check, exposing MatchString for parity with other local validators in this file.
var validFKReference = fkRefMatcher{}

type fkRefMatcher struct{}

func (fkRefMatcher) MatchString(s string) bool {
	return security.ValidateFKReference(s) == nil
}

// parseColumnDef parses a column value that is either a simple string type
// ("TEXT") or an object with constraints ({"type":"TEXT","not_null":true,...}).
func parseColumnDef(name string, raw any) (*schemaColumnDef, error) {
	cd := &schemaColumnDef{Name: name}

	switch v := raw.(type) {
	case string:
		// Simple format: "TEXT"
		cd.Type = strings.ToUpper(v)
	case map[string]any:
		// Extended format: {"type":"TEXT","not_null":true,"unique":true,"default":"'draft'","check":"> 0"}
		typeStr, _ := v["type"].(string)
		if typeStr == "" {
			return nil, fmt.Errorf("column %s: 'type' is required in object format", name)
		}
		cd.Type = strings.ToUpper(typeStr)
		if nn, ok := v["not_null"].(bool); ok {
			cd.NotNull = nn
		}
		if uq, ok := v["unique"].(bool); ok {
			cd.Unique = uq
		}
		if def, ok := v["default"].(string); ok && def != "" {
			cd.Default = def
		} else if defNum, ok := v["default"].(float64); ok {
			// JSON numbers come as float64, format as integer if whole, else float.
			if defNum == float64(int64(defNum)) {
				cd.Default = fmt.Sprintf("%d", int64(defNum))
			} else {
				cd.Default = fmt.Sprintf("%g", defNum)
			}
		} else if defBool, ok := v["default"].(bool); ok {
			// JSON booleans (e.g. {"default":false}) arrive as Go bool, matching neither
			// the string nor float64 branch. Without this case the default is dropped and
			// omitted inserts store NULL instead of 0/1.
			if defBool {
				cd.Default = "1"
			} else {
				cd.Default = "0"
			}
		}
		if gen, ok := v["generate"].(string); ok && strings.TrimSpace(gen) != "" {
			if cd.Default != "" {
				return nil, fmt.Errorf("column %s: set either 'default' or 'generate', not both", name)
			}
			expr, gerr := generatorDefaultSQL(gen)
			if gerr != nil {
				return nil, fmt.Errorf("column %s: %w", name, gerr)
			}
			cd.DefaultExpr = expr
		}
		if chk, ok := v["check"].(string); ok && chk != "" {
			if err := validateCheckExpr(name, chk); err != nil {
				return nil, err
			}
			cd.Check = chk
		}
		if ref, ok := v["references"].(string); ok && ref != "" {
			if !validFKReference.MatchString(ref) {
				return nil, fmt.Errorf("column %s: invalid references format '%s', use 'table(column)' (e.g. 'users(id)')", name, ref)
			}
			cd.References = ref
		}
	default:
		return nil, fmt.Errorf("column %s: type must be a string or object", name)
	}

	sqlType, allowed := allowedColumnTypes[cd.Type]
	if !allowed {
		return nil, fmt.Errorf("unsupported column type: %s (allowed: TEXT, INTEGER, REAL, BOOLEAN, BLOB, PASSWORD, ENCRYPTED)", cd.Type)
	}
	cd.SQLType = sqlType

	// Checked once SQLType is known, because what a DEFAULT may be depends on it (a numeric
	// column's default is emitted unquoted by quoteDefaultValue). Covers every branch above,
	// including the Go-formatted JSON number/bool defaults, which pass by construction.
	if err := validateDefaultLiteral(name, cd.Default, cd.SQLType); err != nil {
		return nil, err
	}

	if kind, isSecure := store.SecureColumnKinds[cd.Type]; isSecure {
		cd.IsSecure = true
		cd.Kind = kind
	}

	// Generated columns (code/token/uuid/expires_*) always produce text values,
	// so they require a TEXT column.
	if cd.DefaultExpr != "" && cd.Type != "TEXT" {
		return nil, fmt.Errorf("column %s: generated columns must be TEXT (got %s)", name, cd.Type)
	}

	return cd, nil
}

// columnSQL returns the SQL fragment for a column definition including constraints.
func (cd *schemaColumnDef) columnSQL() string {
	var parts []string
	parts = append(parts, cd.Name, cd.SQLType)
	if cd.NotNull {
		parts = append(parts, "NOT NULL")
	}
	if cd.DefaultExpr != "" {
		// A generated default is a fixed, injection-safe SQL expression built by
		// generatorDefaultSQL - emit it verbatim (never quoted as a literal).
		parts = append(parts, "DEFAULT "+cd.DefaultExpr)
	} else if cd.Default != "" {
		parts = append(parts, "DEFAULT "+quoteDefaultValue(cd.Default, cd.SQLType))
	}
	if cd.Check != "" {
		parts = append(parts, fmt.Sprintf("CHECK (%s)", normalizeCheckExpr(cd.Name, cd.Check)))
	}
	if cd.References != "" {
		parts = append(parts, "REFERENCES "+cd.References)
	}
	return strings.Join(parts, " ")
}

// normalizeCheckExpr builds the inside of a CHECK (...) clause from a column's raw check
// expression, which arrives in one of two shapes:
//
//	bare fragment  - "> 0", "IN ('a','b')" : operator + operand, column implied;
//	                 the column name is prepended -> "col > 0".
//	full predicate - "col IN (...)", "length(name) > 0" : already complete, passes through.
//
// The bare-fragment case is detected by its leading operator; prepending the column to an
// already-complete predicate would duplicate the reference.
func normalizeCheckExpr(col, check string) string {
	t := strings.TrimSpace(check)
	if t == "" {
		return col
	}
	// A leading comparison operator (<, >, =, !) means the column reference is
	// implied and must be prepended.
	if strings.ContainsAny(t[:1], "<>=!") {
		return col + " " + t
	}
	// A leading predicate keyword (IN/NOT/LIKE/GLOB/BETWEEN/IS) is the other
	// bare-fragment form. Take the first word, split on space or '(' so
	// "IN('a')" is still recognized.
	words := strings.FieldsFunc(t, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '('
	})
	if len(words) > 0 {
		switch strings.ToUpper(words[0]) {
		case "IN", "NOT", "LIKE", "GLOB", "BETWEEN", "IS":
			return col + " " + t
		}
	}
	// Anything else is already a full expression; emit it verbatim.
	return t
}

// generatorDefaultSQL maps a declarative `generate` kind to an injection-SAFE SQLite
// DEFAULT expression, emitted verbatim by columnSQL. Only these fixed templates are
// accepted (arbitrary expressions never are, which is why a plain `default` is always
// quoted as a literal), so a CRUD auto-insert can fill a server-generated value with no
// server function.
//
// Kinds:
//
//	code                -> 8 random hex chars   (short share link / slug)
//	token               -> 32 random hex chars  (opaque invite/reset token)
//	uuid                -> dashed 8-4-4-4-12 random hex (v4-shaped id)
//	expires_in_days:N   -> datetime('now','+N days')
//	expires_in_hours:N  -> datetime('now','+N hours')
func generatorDefaultSQL(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "code":
		return "(lower(hex(randomblob(4))))", nil
	case "token":
		return "(lower(hex(randomblob(16))))", nil
	case "uuid":
		return "(lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-' || hex(randomblob(2)) || '-' || hex(randomblob(2)) || '-' || hex(randomblob(6))))", nil
	}
	k := strings.ToLower(strings.TrimSpace(kind))
	for _, e := range []struct{ prefix, unit string }{
		{"expires_in_days:", "days"},
		{"expires_in_hours:", "hours"},
	} {
		if strings.HasPrefix(k, e.prefix) {
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(k, e.prefix)))
			if err != nil || n <= 0 {
				return "", fmt.Errorf("generate %q needs a positive integer, e.g. expires_in_%s:7", kind, e.unit)
			}
			if n > 100000 {
				return "", fmt.Errorf("generate %q offset is too large (max 100000)", kind)
			}
			// n is a validated integer, so interpolation is injection-safe.
			return fmt.Sprintf("(datetime('now','+%d %s'))", n, e.unit), nil
		}
	}
	return "", fmt.Errorf("unknown generate kind %q (use: code, token, uuid, expires_in_days:N, expires_in_hours:N)", kind)
}

// quoteDefaultValue ensures default values are safe for SQL interpolation.
// TEXT/BOOLEAN columns get their defaults single-quoted unless already quoted
// or a recognized SQL keyword. Numeric types pass through as-is.
func quoteDefaultValue(val, sqlType string) string {
	// Already a SQL expression (quoted, numeric, or keyword).
	if strings.HasPrefix(val, "'") || strings.HasPrefix(val, "\"") {
		return val
	}
	upper := strings.ToUpper(val)
	if upper == "NULL" || upper == "CURRENT_TIMESTAMP" || upper == "TRUE" || upper == "FALSE" {
		return val
	}

	switch sqlType {
	case "INTEGER", "REAL", "BOOLEAN":
		// Numeric/boolean defaults should be bare (0/1), pass through. TRUE/FALSE
		// keyword forms are already handled by the keyword check above.
		return val
	default:
		// TEXT, BOOLEAN, and other types: single-quote the value.
		// Escape internal single quotes to prevent SQL injection.
		escaped := strings.ReplaceAll(val, "'", "''")
		return "'" + escaped + "'"
	}
}

// (systemTables / IsSystemTable / SanitizedTableName / AllowedSQLOps /
// BuildWhereClause / ValidTableName / ValidColumnName live in tools/store.
// Schema handlers below call store.X; this keeps the cross-family contract
// in one place.)

// manage_schema handler functions are below, they are registered as actions
// inside manage_data (create_table, alter_table, describe_table, list_tables,
// drop_table, add_index, set_validation).

func SchemaCreate(ctx *tools.ToolContext, tableName string, columnsRaw map[string]any, searchableColumns []any) (*tools.Result, error) {
	if tableName == "" {
		return tools.Fail("create_table requires a table_name"), nil
	}
	if len(columnsRaw) == 0 {
		// Name was given but no columns: the common case is an empty `column_defs: {}`.
		// Say so precisely rather than naming table_name too, which reads as if the name
		// were missing.
		return tools.Fail(
			"create_table for %q needs at least one column in column_defs (id and created_at are added automatically) - define the columns, or drop this create_table if the table isn't needed",
			tableName), nil
	}

	// Skip if table already exists (prevents duplicate creation during BUILD).
	var existingCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&existingCount); err == nil && existingCount > 0 {
		return tools.Ok(map[string]any{
			"table_name":      tableName,
			"already_existed": true,
			"message":         "Table " + tableName + " already exists, so nothing was created and NO column in this call was applied. Add columns with manage_schema action=alter_table; inspect the current shape with action=describe_table.",
		}), nil
	}

	physicalName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Validate and build column definitions, track secure columns.
	var colDefs []string
	var uniqueCols []string
	secureCols := map[string]string{}

	for colName, colTypeRaw := range columnsRaw {
		// Skip reserved columns that are auto-added by the CREATE TABLE template.
		if strings.EqualFold(colName, "id") || strings.EqualFold(colName, "created_at") {
			continue
		}
		if err := security.ValidateColumnName(colName); err != nil {
			return tools.Fail("%s", err.Error()), nil
		}

		cd, err := parseColumnDef(colName, colTypeRaw)
		if err != nil {
			return tools.Fail("%s", err.Error()), nil
		}

		colDefs = append(colDefs, cd.columnSQL())

		if cd.IsSecure {
			secureCols[colName] = cd.Kind
		}
		if cd.Unique {
			uniqueCols = append(uniqueCols, colName)
		}
	}

	createSQL := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id INTEGER PRIMARY KEY AUTOINCREMENT, %s, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)",
		physicalName, strings.Join(colDefs, ", "),
	)

	// Defense in depth: every column fragment is validated at parse time, so this only
	// fires if a constraint path composes DDL without that validation. Worth asserting
	// because the driver would execute an extra statement rather than reject it.
	if sqlguard.HasStackedStatements(createSQL) {
		return nil, fmt.Errorf("refusing to execute multi-statement CREATE TABLE for %s", tableName)
	}
	if _, err := ctx.DB.Exec(createSQL); err != nil {
		return nil, fmt.Errorf("creating table: %w", err)
	}

	// Record the table in of_dynamic_tables registry with secure column info.
	schemaDef, err := json.Marshal(columnsRaw)
	if err != nil {
		return nil, fmt.Errorf("marshaling schema: %w", err)
	}
	secureColsJSON, err := json.Marshal(secureCols)
	if err != nil {
		return nil, fmt.Errorf("marshaling secure columns: %w", err)
	}
	// Extract per-column validation rules from column definitions.
	validationRules := map[string]any{}
	for colName, colTypeRaw := range columnsRaw {
		if obj, ok := colTypeRaw.(map[string]any); ok {
			if v, ok := obj["validation"].(map[string]any); ok && len(v) > 0 {
				validationRules[colName] = v
			}
		}
	}
	validationJSON, _ := json.Marshal(validationRules)

	_, err = ctx.DB.Exec(
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns, validation)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(table_name) DO UPDATE SET schema_def = excluded.schema_def, secure_columns = excluded.secure_columns, validation = excluded.validation`,
		tableName, string(schemaDef), string(secureColsJSON), string(validationJSON),
	)
	if err != nil {
		return nil, fmt.Errorf("recording dynamic table: %w", err)
	}

	// Create UNIQUE indexes for columns marked unique.
	for _, col := range uniqueCols {
		if err := createUniqueIndex(ctx.DB, physicalName, col); err != nil {
			return nil, err
		}
	}

	// Create FTS5 full-text search index if searchable_columns provided.
	var searchableCols []string
	var skippedSearchable []string
	if len(searchableColumns) > 0 {
		for _, sc := range searchableColumns {
			colName, ok := sc.(string)
			if !ok || !store.ValidColumnName.MatchString(colName) {
				if ok {
					skippedSearchable = append(skippedSearchable, colName)
				}
				continue
			}
			// Verify the column exists and is TEXT type (supports both string and object formats).
			isText := false
			if colVal, exists := columnsRaw[colName]; exists {
				switch cv := colVal.(type) {
				case string:
					isText = strings.EqualFold(cv, "TEXT")
				case map[string]any:
					if ct, ok := cv["type"].(string); ok {
						isText = strings.EqualFold(ct, "TEXT")
					}
				}
			}
			if isText {
				searchableCols = append(searchableCols, colName)
			} else {
				// Either the column isn't declared or it isn't TEXT; FTS only
				// indexes TEXT columns, so flag it instead of silently dropping.
				skippedSearchable = append(skippedSearchable, colName)
			}
		}
		if len(searchableCols) > 0 {
			if err := CreateFTSIndex(ctx.DB, physicalName, searchableCols); err != nil {
				// Non-fatal, FTS is a bonus, not a requirement.
				return tools.Ok(map[string]any{
					"table":          tableName,
					"columns":        columnsRaw,
					"secure_columns": secureCols,
					"fts_warning":    fmt.Sprintf("FTS5 index creation failed: %v", err),
				}), nil
			}
		}
	}

	resultData := map[string]any{
		"table":          tableName,
		"columns":        columnsRaw,
		"secure_columns": secureCols,
	}
	// Read the table back and return the live schema - actual columns, types, NOT NULL and
	// defaults as created (including the implicit id/created_at) - so the caller confirms
	// the table matches intent in one shot without a separate probe.
	if liveCols, err := store.GetTableColumnInfo(ctx.DB, physicalName); err == nil {
		resultData["schema"] = liveCols
	}
	if len(searchableCols) > 0 {
		resultData["fts_columns"] = searchableCols
	}
	if len(skippedSearchable) > 0 {
		resultData["searchable_columns_skipped"] = skippedSearchable
		resultData["searchable_warning"] = fmt.Sprintf(
			"these searchable_columns were not indexed (not declared, or not TEXT): %s", strings.Join(skippedSearchable, ", "))
	}

	// Publish schema.created event.
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventSchemaCreated, ctx.ProjectID, map[string]any{
			"table": tableName,
		}))
	}

	return tools.Ok(resultData), nil
}

// lowerCaseTableColumns returns the table's column names lowercased, for the
// case-insensitive matching alter-table reconciliation needs. Distinct from
// store.GetTableColumns, which preserves case for callers that render columns to the user.
func lowerCaseTableColumns(db dbh.Handle, physicalName string) (map[string]bool, error) {
	cols, err := store.GetTableColumns(db, physicalName)
	if err != nil {
		return map[string]bool{}, err
	}
	lower := make(map[string]bool, len(cols))
	for name := range cols {
		lower[strings.ToLower(name)] = true
	}
	return lower, nil
}

// createUniqueIndex creates an idempotent UNIQUE index on a single column of
// physicalName, shared by SchemaCreate and SchemaAlter.
func createUniqueIndex(db dbh.Handle, physicalName, col string) error {
	idxName := fmt.Sprintf("idx_%s_%s_unique", physicalName, col)
	idxSQL := fmt.Sprintf("CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s (%s)", idxName, physicalName, col)
	if _, err := db.Exec(idxSQL); err != nil {
		return fmt.Errorf("creating unique index on %s: %w", col, err)
	}
	return nil
}

func SchemaAlter(ctx *tools.ToolContext, tableName string, addColumns map[string]any, dropColumns []any) (*tools.Result, error) {
	if tableName == "" {
		return tools.Fail("table_name is required"), nil
	}

	physicalName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// A name in both lists would be added and then dropped in one call: the column
	// ends up gone while the result reports an add. alter_table has no rename or
	// retype, so name the real remedy instead of running a call that undoes itself.
	for _, d := range dropColumns {
		if name, ok := d.(string); ok {
			for add := range addColumns {
				if strings.EqualFold(add, name) {
					return tools.FailCode("bad_arg", "column %q is in both add_columns and drop_columns, and alter_table cannot rename or retype a column. To change a column on a table holding rows: add the corrected column under a NEW name, copy the values across with manage_data or manage_sql, then drop the old one in a later call. On an empty table drop_table and create_table again.", name), nil
				}
			}
		}
	}

	// Load existing secure columns.
	secureCols, _ := loadSecureColumns(ctx, tableName)
	if secureCols == nil {
		secureCols = map[string]string{}
	}

	// Snapshot existing columns once so add/drop can be idempotent, the LLM
	// often re-issues alter calls (especially after a build resume) and a hard
	// "duplicate column name" error tanks the build for no reason.
	existingCols, _ := lowerCaseTableColumns(ctx.DB, physicalName)

	var addedCols []string
	var skippedAdds []string
	var droppedCols []string
	var skippedDrops []string
	// ftsWarning surfaces a non-fatal post-ALTER failure to the caller instead of only
	// logging it (the columns are already dropped and the ALTER is non-transactional, so no
	// rollback), so a broken search index isn't reported as a clean success.
	var ftsWarning string

	// Add columns.
	if len(addColumns) > 0 {
		for colName, colTypeRaw := range addColumns {
			if err := security.ValidateColumnName(colName); err != nil {
				return tools.Fail("%s", err.Error()), nil
			}

			// Idempotent: if the column already exists, skip it.
			if existingCols[strings.ToLower(colName)] {
				skippedAdds = append(skippedAdds, colName)
				continue
			}

			cd, err := parseColumnDef(colName, colTypeRaw)
			if err != nil {
				return tools.Fail("%s", err.Error()), nil
			}
			if cd.DefaultExpr != "" {
				// SQLite forbids a non-constant (expression) DEFAULT on ALTER TABLE
				// ADD COLUMN, so a generated column can only be declared at create
				// time. Surface that clearly instead of a raw SQLite error.
				return tools.Fail(
					"column %q uses generate/expression default, which SQLite cannot ADD to an existing table - declare it in the original create_table, or backfill it with a manage_function / UPDATE", colName), nil
			}

			alterSQL := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", physicalName, cd.columnSQL())
			// Defense in depth, as in create_table above.
			if sqlguard.HasStackedStatements(alterSQL) {
				return nil, fmt.Errorf("refusing to execute multi-statement ALTER TABLE for column %s", colName)
			}
			if _, err := ctx.DB.Exec(alterSQL); err != nil {
				return nil, fmt.Errorf("adding column %s: %w", colName, err)
			}

			// Track secure columns.
			if cd.IsSecure {
				secureCols[colName] = cd.Kind
			}

			// Create UNIQUE index if requested.
			if cd.Unique {
				if err := createUniqueIndex(ctx.DB, physicalName, colName); err != nil {
					return nil, err
				}
			}

			existingCols[strings.ToLower(colName)] = true
			addedCols = append(addedCols, colName)
		}
	}

	// Drop columns (SQLite supports ALTER TABLE DROP COLUMN since 3.35.0).
	if len(dropColumns) > 0 {
		protectedCols := map[string]bool{"id": true, "created_at": true}

		// Collect valid column names first for the approval check.
		var colsToDrop []string
		for _, colRaw := range dropColumns {
			colName, ok := colRaw.(string)
			if !ok || colName == "" {
				continue
			}
			if protectedCols[colName] {
				return tools.Fail("cannot drop protected column: %s", colName), nil
			}
			// Idempotent: if the column doesn't exist, skip it.
			if !existingCols[strings.ToLower(colName)] {
				skippedDrops = append(skippedDrops, colName)
				continue
			}
			colsToDrop = append(colsToDrop, colName)
		}

		// Approval gate for dropping columns (data loss). An empty table has nothing to
		// lose, so it needs no owner decision (the same waiver SchemaDrop applies). A count
		// that fails leaves the table not known to be empty, so the gate holds.
		var rowCount int
		countErr := ctx.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", physicalName)).Scan(&rowCount)
		isEmpty := countErr == nil && rowCount == 0

		if len(colsToDrop) > 0 && !isEmpty {
			approvalKey := fmt.Sprintf("drop_columns:%s:%s", tableName, strings.Join(colsToDrop, ","))
			if !tools.CheckApproval(ctx.DB, approvalKey) {
				question := fmt.Sprintf(
					"The AI wants to drop column(s) %s from table '%s'. Data in these columns will be permanently lost. Approve?",
					strings.Join(colsToDrop, ", "), tableName,
				)
				return tools.RequestApproval(ctx, approvalKey, question,
					fmt.Sprintf("Dropping columns [%s] from table '%s'.", strings.Join(colsToDrop, ", "), tableName),
				), nil
			}
		}

		// Before dropping columns, check if FTS triggers exist that reference them.
		// FTS sync triggers hard-code column names, they must be dropped first
		// and recreated with the remaining columns after the ALTER completes.
		ftsColsBefore := getFTSColumns(ctx.DB, physicalName)

		for _, colName := range colsToDrop {
			alterSQL := fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", physicalName, colName)
			if _, err := ctx.DB.Exec(alterSQL); err != nil {
				return nil, fmt.Errorf("dropping column %s: %w", colName, err)
			}

			// Remove from secure columns if present.
			delete(secureCols, colName)
			droppedCols = append(droppedCols, colName)
		}

		// Rebuild FTS triggers if any dropped columns were in the FTS index.
		if len(ftsColsBefore) > 0 {
			droppedSet := make(map[string]bool, len(droppedCols))
			for _, c := range droppedCols {
				droppedSet[c] = true
			}
			// Drop existing triggers, since they reference now-removed columns.
			// Best-effort: a leftover trigger surfaces via the rebuild below.
			_, _ = ctx.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s_ai", physicalName))
			_, _ = ctx.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s_ad", physicalName))
			_, _ = ctx.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s_au", physicalName))
			// Compute remaining FTS columns.
			var remaining []string
			for _, c := range ftsColsBefore {
				if !droppedSet[c] {
					remaining = append(remaining, c)
				}
			}
			if len(remaining) == 0 {
				// No searchable columns left, drop FTS table entirely (best-effort).
				_, _ = ctx.DB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s_fts", physicalName))
			} else {
				// Rebuild FTS table and triggers with remaining columns.
				_, _ = ctx.DB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s_fts", physicalName))
				if err := CreateFTSIndex(ctx.DB, physicalName, remaining); err != nil {
					ctx.Logger.Error("failed to rebuild FTS after column drop", "table", tableName, "error", err)
					ftsWarning = fmt.Sprintf("full-text search index for '%s' could not be rebuilt after the column drop and is now disabled (%v); re-run alter or recreate the index to restore search", tableName, err)
				}
			}
		}

		if len(droppedCols) > 0 {
			tools.LogDestructiveAction(ctx.DB, "manage_schema", "alter_table_drop_columns",
				fmt.Sprintf("%s:[%s]", tableName, strings.Join(droppedCols, ",")))
		}
	}

	if len(addedCols) == 0 && len(droppedCols) == 0 {
		// Nothing changed, but if every requested add/drop was skipped as already in the
		// desired state, that's an idempotent no-op success, not a failure. Only error when
		// the caller passed no columns at all.
		if len(skippedAdds) == 0 && len(skippedDrops) == 0 {
			return tools.Fail("no columns to add or drop"), nil
		}
		return tools.Ok(map[string]any{
			"table_name":    tableName,
			"skipped_adds":  skippedAdds,
			"skipped_drops": skippedDrops,
			"message":       "All requested columns were already in the desired state, nothing to alter.",
		}), nil
	}

	// Update secure_columns in the registry, fail loudly since losing
	// security metadata means future inserts may store passwords/secrets as plaintext.
	secureColsJSON, err := json.Marshal(secureCols)
	if err != nil {
		return tools.Fail("failed to marshal secure columns: %v", err), nil
	}
	if _, err := ctx.DB.Exec(
		"UPDATE of_dynamic_tables SET secure_columns = ? WHERE table_name = ?",
		string(secureColsJSON), tableName,
	); err != nil {
		return tools.Fail("failed to update secure columns registry: %v", err), nil
	}

	// Publish schema.altered event.
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventSchemaAltered, ctx.ProjectID, map[string]any{
			"table":   tableName,
			"added":   addedCols,
			"dropped": droppedCols,
		}))
	}

	data := map[string]any{
		"table":   tableName,
		"added":   addedCols,
		"dropped": droppedCols,
	}
	if len(skippedAdds) > 0 {
		data["skipped_adds"] = skippedAdds
	}
	if len(skippedDrops) > 0 {
		data["skipped_drops"] = skippedDrops
	}
	if ftsWarning != "" {
		data["warning"] = ftsWarning
	}
	return tools.Ok(data), nil
}

func SchemaDescribe(ctx *tools.ToolContext, tableName string) (*tools.Result, error) {
	if tableName == "" {
		return tools.Fail("table_name is required"), nil
	}

	physicalName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Load secure columns and foreign keys BEFORE opening the PRAGMA
	// table_info cursor. ctx.DB has MaxOpenConns=1; opening a cursor
	// and then querying the same pool deadlocks.
	secureCols, _ := loadSecureColumns(ctx, tableName)

	// Build column->"table(column)" map from PRAGMA foreign_key_list.
	fkMap := map[string]string{}
	if fkRows, fkErr := ctx.DB.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", physicalName)); fkErr == nil {
		for fkRows.Next() {
			var id, seq int
			var refTable, from, to, onUpdate, onDelete, match string
			if fkRows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match) == nil {
				fkMap[from] = fmt.Sprintf("%s(%s)", refTable, to)
			}
		}
		_ = fkRows.Err() // advisory enrichment; tolerated like the Query error
		fkRows.Close()
	}

	// Get column info via PRAGMA.
	rows, err := ctx.DB.Query(fmt.Sprintf("PRAGMA table_info(%s)", physicalName))
	if err != nil {
		return nil, fmt.Errorf("describing table: %w", err)
	}
	defer rows.Close()

	var columns []map[string]any
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			continue
		}

		col := map[string]any{
			"name": name,
			"type": colType,
		}
		if pk == 1 {
			col["primary_key"] = true
		}
		if notNull == 1 {
			col["not_null"] = true
		}
		if dfltValue != nil {
			col["default"] = dfltValue
		}
		if secureCols != nil {
			if kind, ok := secureCols[name]; ok {
				switch kind {
				case "hash":
					col["type"] = "PASSWORD"
					col["secure"] = "PASSWORD"
				case "encrypt":
					col["type"] = "ENCRYPTED"
					col["secure"] = "ENCRYPTED"
				}
			}
		}
		if ref, ok := fkMap[name]; ok {
			col["references"] = ref
		}
		columns = append(columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("describing table: %w", err)
	}

	if len(columns) == 0 {
		return tools.NotFound("table", tableName), nil
	}

	// Index list via PRAGMA index_list, then per-index columns via PRAGMA index_info.
	type indexInfo struct {
		Name    string   `json:"name"`
		Unique  bool     `json:"unique"`
		Columns []string `json:"columns"`
		Origin  string   `json:"origin,omitempty"` // c=created, u=unique, pk=primary key
	}
	var indices []indexInfo
	if idxRows, idxErr := ctx.DB.Query(fmt.Sprintf("PRAGMA index_list(%s)", physicalName)); idxErr == nil {
		var listed []indexInfo
		for idxRows.Next() {
			var seq, unique, partial int
			var name, origin string
			if idxRows.Scan(&seq, &name, &unique, &origin, &partial) == nil {
				listed = append(listed, indexInfo{
					Name:   name,
					Unique: unique == 1,
					Origin: origin,
				})
			}
		}
		_ = idxRows.Err() // advisory enrichment; tolerated like the Query error
		idxRows.Close()
		for k := range listed {
			cols, cerr := ctx.DB.Query(fmt.Sprintf("PRAGMA index_info(%s)", listed[k].Name))
			if cerr != nil {
				continue
			}
			for cols.Next() {
				var seqno, cid int
				var cname string
				if cols.Scan(&seqno, &cid, &cname) == nil {
					listed[k].Columns = append(listed[k].Columns, cname)
				}
			}
			_ = cols.Err() // advisory enrichment; tolerated like the Query error
			cols.Close()
		}
		indices = listed
	}

	// Row count, informational, not transactional. Cheap on small tables.
	var rowCount int64
	_ = ctx.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", physicalName)).Scan(&rowCount)

	out := map[string]any{
		"table":     tableName,
		"columns":   columns,
		"indices":   indices,
		"row_count": rowCount,
	}
	if rules := store.LoadValidationRules(ctx.DB, tableName); len(rules) > 0 {
		out["validation"] = rules
	}
	return tools.Ok(out), nil
}

func SchemaList(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT table_name, schema_def, created_at FROM of_dynamic_tables ORDER BY table_name",
	)
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}

	type tableEntry struct {
		Name      string `json:"table_name"`
		Schema    string `json:"schema"`
		CreatedAt any    `json:"created_at"`
	}
	var collected []tableEntry
	for rows.Next() {
		var t tableEntry
		if err := rows.Scan(&t.Name, &t.Schema, &t.CreatedAt); err != nil {
			continue
		}
		collected = append(collected, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}

	// Now that the cursor is closed, fetch per-table row counts. ctx.DB has
	// MaxOpenConns=1, so the count query would deadlock if we ran it inside
	// the rows.Next() loop above.
	tables := make([]map[string]any, 0, len(collected))
	for _, t := range collected {
		entry := map[string]any{
			"table_name": t.Name,
			"schema":     t.Schema,
			"created_at": t.CreatedAt,
		}
		if physical, perr := store.SanitizedTableName(t.Name); perr == nil {
			var rowCount int64
			if cerr := ctx.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", physical)).Scan(&rowCount); cerr == nil {
				entry["row_count"] = rowCount
			}
		}
		tables = append(tables, entry)
	}

	// Append views (created via create_view) so they're discoverable alongside
	// tables. They live in sqlite_master rather than of_dynamic_tables; tag each
	// with object_type so callers can tell them apart. Collect names first - the
	// MaxOpenConns=1 pool would deadlock if we ran the COUNT inside this cursor.
	if viewRows, verr := ctx.DB.Query(
		"SELECT name FROM sqlite_master WHERE type = 'view' AND name NOT LIKE 'of_%' ORDER BY name",
	); verr == nil {
		var viewNames []string
		for viewRows.Next() {
			var name string
			if viewRows.Scan(&name) == nil {
				viewNames = append(viewNames, name)
			}
		}
		_ = viewRows.Err() // advisory extras; tolerated like the Query error
		viewRows.Close()
		for _, name := range viewNames {
			entry := map[string]any{"table_name": name, "object_type": "view"}
			if store.ValidTableName.MatchString(name) {
				var rowCount int64
				if cerr := ctx.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", name)).Scan(&rowCount); cerr == nil {
					entry["row_count"] = rowCount
				}
			}
			tables = append(tables, entry)
		}
	}

	return tools.Ok(tables), nil
}

func SchemaDrop(ctx *tools.ToolContext, tableName string) (*tools.Result, error) {
	if tableName == "" {
		return tools.Fail("table_name is required"), nil
	}

	physicalName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Verify the table exists in the registry.
	var exists int
	err = ctx.DB.QueryRow(
		"SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?",
		tableName,
	).Scan(&exists)
	if err != nil || exists == 0 {
		return tools.NotFound("table", tableName), nil
	}

	// Count rows: the owner sees the number in the prompt, and an empty table
	// waives the gate below. A count that fails leaves the table not known to be
	// empty, so isEmpty stays false and the gate holds.
	var rowCount int
	countErr := ctx.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", physicalName)).Scan(&rowCount)
	isEmpty := countErr == nil && rowCount == 0

	// --- Approval gate: dropping a table is irreversible ---
	// The gate exists to protect data, so an empty table does not need it: there
	// is nothing to lose, and a column's name or type can only be corrected by
	// recreating the table (alter_table adds and drops only).
	approvalKey := "drop_table:" + tableName
	if !isEmpty && !tools.CheckApproval(ctx.DB, approvalKey) {
		question := fmt.Sprintf(
			"The AI wants to DROP the table '%s' (%d rows). This will permanently delete all data in the table and remove related API endpoints. This cannot be undone. Approve?",
			tableName, rowCount,
		)
		return tools.RequestApproval(ctx, approvalKey, question,
			fmt.Sprintf("Dropping table '%s' (%d rows) and its related endpoints.", tableName, rowCount),
		), nil
	}

	// Drop the physical table.
	if _, err := ctx.DB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", physicalName)); err != nil {
		return nil, fmt.Errorf("dropping table: %w", err)
	}

	// Remove from of_dynamic_tables registry.
	if _, err := ctx.DB.Exec("DELETE FROM of_dynamic_tables WHERE table_name = ?", tableName); err != nil {
		return nil, fmt.Errorf("removing table registry entry: %w", err)
	}

	// Collect the endpoint paths bound to this table before deleting their
	// config rows, so the matching of_endpoints spine rows are released too.
	// Deleting only the config rows leaves orphan spine routes that answer
	// 405/500 after the table is gone.
	apiPaths := endpointPathsForTable(ctx.DB, "of_api_endpoints", tableName)
	authPaths := endpointPathsForTable(ctx.DB, "of_auth_endpoints", tableName)

	// Remove related API endpoints.
	if _, err := ctx.DB.Exec("DELETE FROM of_api_endpoints WHERE table_name = ?", tableName); err != nil {
		return nil, fmt.Errorf("removing api endpoints: %w", err)
	}

	// Remove related auth endpoints.
	if _, err := ctx.DB.Exec("DELETE FROM of_auth_endpoints WHERE table_name = ?", tableName); err != nil {
		return nil, fmt.Errorf("removing auth endpoints: %w", err)
	}

	// Release the spine rows for the endpoints just removed.
	for _, p := range apiPaths {
		_ = api.ReleaseEndpoint(ctx, endpoints.KindCRUD, p)
	}
	for _, p := range authPaths {
		_ = api.ReleaseEndpoint(ctx, endpoints.KindAuth, p)
	}

	tools.LogDestructiveAction(ctx.DB, "manage_schema", "drop_table", tableName)

	return tools.Ok(map[string]any{
		"dropped": tableName,
	}), nil
}

// endpointPathsForTable returns the endpoint paths in configTable bound to
// tableName, so their spine rows can be released when the table drops.
// configTable is a trusted constant at the call sites; tableName is parameterized.
func endpointPathsForTable(db dbh.Handle, configTable, tableName string) []string {
	rows, err := db.Query("SELECT path FROM "+configTable+" WHERE table_name = ?", tableName)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return paths
		}
		paths = append(paths, p)
	}
	_ = rows.Err()
	return paths
}

func SchemaSummarize(result string) string {
	return tools.SummarizeFrame(result, "Listed %d tables", func(dataMap map[string]any) string {
		if tableName, _ := dataMap["table"].(string); tableName != "" {
			// describe_table lists its columns; create_table echoes its column_defs map.
			// Any other result carrying "table" (count, aggregate, alter_table, an
			// export) keeps its own compact JSON rather than being relabelled "created"
			// in the history the model rereads.
			if cols, ok := dataMap["columns"].([]any); ok {
				return fmt.Sprintf(`{"success":true,"summary":"Table %s: %d columns"}`, tableName, len(cols))
			}
			if _, ok := dataMap["columns"].(map[string]any); ok {
				return fmt.Sprintf(`{"success":true,"summary":"Table %s created"}`, tableName)
			}
		}
		return ""
	})
}

// Secure-column helpers

// loadSecureColumns loads the secure_columns JSON map for a dynamic table.
// Convenience wrapper around the exported LoadSecureColumns for in-package
// callers that already hold a *tools.ToolContext.
func loadSecureColumns(ctx *tools.ToolContext, tableName string) (map[string]string, error) {
	return store.LoadSecureColumns(ctx.DB, tableName)
}

// loadSecureColumnsTx is the transaction-aware sibling: when the caller is
// dispatching inside an outer tx (manage_sql's transaction action), the lookup
// is routed through the tx so it doesn't deadlock waiting for the connection
// the tx already holds. Falls back to ctx.DB on a nil tx.
func loadSecureColumnsTx(ctx *tools.ToolContext, tx *sql.Tx, tableName string) (map[string]string, error) {
	if tx != nil {
		var raw string
		err := tx.QueryRow(
			"SELECT secure_columns FROM of_dynamic_tables WHERE table_name = ?",
			tableName,
		).Scan(&raw)
		if err != nil {
			return nil, nil
		}
		var cols map[string]string
		if err := json.Unmarshal([]byte(raw), &cols); err != nil {
			return nil, nil
		}
		return cols, nil
	}
	return store.LoadSecureColumns(ctx.DB, tableName)
}

// (LoadSecureColumns, LoadTableSchema, ProcessSecureValue moved to tools/store.)
