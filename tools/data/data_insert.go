package data

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"openfabrica/internal/dbh"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

// preflightRequiredColumns returns a human-readable error when any row in a
// bulk insert omits a required (NOT NULL, no-default) column, or "" when every
// row is complete. It reports all offending rows together (a raw constraint
// error stops at the first) and adds a "looks truncated" hint for a row that
// supplies far fewer columns than its siblings - the signature of a malformed
// row the model emitted, e.g. a partial first object with only a clipped body.
func preflightRequiredColumns(db dbh.Handle, physicalName string, rowsRaw []any) string {
	required, err := store.RequiredColumns(db, physicalName)
	if err != nil || len(required) == 0 {
		return ""
	}

	// widest = the column count a complete row is expected to have; used only
	// to decide whether a sparse row looks truncated.
	widest := 0
	for _, rr := range rowsRaw {
		if row, ok := rr.(map[string]any); ok && len(row) > widest {
			widest = len(row)
		}
	}

	var report []string
	for i, rr := range rowsRaw {
		row, ok := rr.(map[string]any)
		if !ok {
			continue // non-object rows are caught in the insert loop
		}
		var missing []string
		for col := range required {
			if v, present := row[col]; !present || v == nil {
				missing = append(missing, col)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		entry := fmt.Sprintf("row %d missing required column(s): %s", i, strings.Join(missing, ", "))
		if widest >= 3 && len(row) <= widest/2 {
			entry += fmt.Sprintf(" (only %d of the %d columns the other rows set - it looks truncated; re-send this row complete)", len(row), widest)
		}
		report = append(report, entry)
	}
	if len(report) == 0 {
		return ""
	}
	return "insert aborted - every row must include all NOT NULL columns that have no default. " + strings.Join(report, "; ")
}

// fkRef describes one foreign key: column from_col references ref_table(ref_col).
type fkRef struct {
	fromCol, refTable, refCol string
}

// fkKey normalises a value for set membership so a JSON number (float64) and the
// integer it represents key the same - 22 and 22.0 are the same id.
func fkKey(v any) string {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%v", v)
}

// preflightForeignKeys returns a human-readable error when any row in a bulk
// insert sets a foreign-key column to a value with no matching parent row, or
// "" when every reference resolves. A raw FK violation reports only an opaque
// SQLite rowid ("inserting row 81: FOREIGN KEY constraint failed"); this names
// the column, the unmatched value, and the parent table so a failed seed is
// debuggable in one shot. Best-effort: any introspection error returns "" and
// lets the insert proceed to the real constraint check.
func preflightForeignKeys(db dbh.Handle, physicalName string, rowsRaw []any) string {
	fkRows, err := db.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", physicalName))
	if err != nil {
		return ""
	}
	var fks []fkRef
	for fkRows.Next() {
		var id, seq int
		var refTable, from, to, onUpdate, onDelete, match string
		if err := fkRows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			fkRows.Close()
			return ""
		}
		if to == "" {
			to = "id" // FK that names no parent column targets the PK; OF tables use id
		}
		fks = append(fks, fkRef{fromCol: from, refTable: refTable, refCol: to})
	}
	fkRows.Close()
	if fkRows.Err() != nil {
		return "" // introspection failure - defer to the real constraint check
	}
	if len(fks) == 0 {
		return ""
	}

	var report []string
	for _, fk := range fks {
		// Collect the distinct non-null values this batch references.
		want := map[string]any{}
		for _, rr := range rowsRaw {
			row, ok := rr.(map[string]any)
			if !ok {
				continue
			}
			if v, present := row[fk.fromCol]; present && v != nil {
				want[fkKey(v)] = v
			}
		}
		if len(want) == 0 {
			continue
		}
		// Which of those values actually exist in the parent table?
		placeholders := make([]string, 0, len(want))
		argsIn := make([]any, 0, len(want))
		for _, v := range want {
			placeholders = append(placeholders, "?")
			argsIn = append(argsIn, v)
		}
		q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)",
			security.QuoteIdent(fk.refCol), security.QuoteIdent(fk.refTable),
			security.QuoteIdent(fk.refCol), strings.Join(placeholders, ", "))
		existRows, err := db.Query(q, argsIn...)
		if err != nil {
			return "" // can't verify (e.g. parent not a real table) - defer to the DB
		}
		present := map[string]bool{}
		for existRows.Next() {
			var v any
			if err := existRows.Scan(&v); err == nil {
				present[fkKey(v)] = true
			}
		}
		existRows.Close()
		if existRows.Err() != nil {
			return "" // can't verify (incomplete parent scan) - defer to the DB
		}

		for i, rr := range rowsRaw {
			row, ok := rr.(map[string]any)
			if !ok {
				continue
			}
			v, has := row[fk.fromCol]
			if !has || v == nil {
				continue
			}
			if !present[fkKey(v)] {
				report = append(report, fmt.Sprintf("row %d: %s.%s=%v has no matching %s.%s",
					i, physicalName, fk.fromCol, v, fk.refTable, fk.refCol))
			}
		}
	}
	if len(report) == 0 {
		return ""
	}
	return "insert aborted - foreign key reference(s) point to rows that don't exist (seed parent tables before children): " + strings.Join(report, "; ")
}

// DataInsert inserts a single row (a.Data) or a bulk batch (a.Rows). tx is the
// outer transaction when dispatched from manage_sql's transaction action, nil
// on a standalone call.
func DataInsert(ctx *tools.ToolContext, a *DataActInsert, tx *sql.Tx) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.Fail("table_name is required"), nil
	}

	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	if len(a.Rows) > 0 && len(a.Data) > 0 {
		return tools.FailCode(tools.CodeBadArg, "pass either data (one row) or rows (many), not both - put the single row inside rows if you meant to insert it too"), nil
	}
	if len(a.Rows) == 0 && len(a.Data) == 0 {
		return tools.Fail("data or rows parameter is required for insert"), nil
	}
	toCheck := []map[string]any{a.Data}
	if len(a.Rows) > 0 {
		toCheck = toCheck[:0]
		for _, r := range a.Rows {
			if m, ok := r.(map[string]any); ok {
				toCheck = append(toCheck, m)
			}
		}
	}
	if r := checkRowValidation(ctx, tx, a.TableName, toCheck); r != nil {
		return r, nil
	}

	// Load secure columns once for all rows. Routed through any outer tx so
	// transaction dispatch doesn't deadlock on the same single-connection pool.
	secureCols, _ := loadSecureColumnsTx(ctx, tx, a.TableName)

	// Bulk insert via "rows" parameter.
	if len(a.Rows) > 0 {
		return DataInsertBulk(ctx, tx, physicalName, a.TableName, a.Rows, secureCols)
	}

	return DataInsertSingle(ctx, tx, physicalName, a.TableName, a.Data, secureCols)
}

func DataInsertSingle(ctx *tools.ToolContext, tx *sql.Tx, physicalName, tableName string, data map[string]any, secureCols map[string]string) (*tools.Result, error) {
	// Same rich pre-flight the bulk path runs, so a single-row insert names the
	// missing NOT NULL column / unmatched FK up front instead of returning the
	// opaque driver error ("NOT NULL constraint failed"). Skipped inside an outer
	// tx (ctx.DB has MaxOpenConns=1, so a PRAGMA cursor would deadlock the held
	// connection) - identical guard to DataInsertBulk.
	if tx == nil {
		oneRow := []any{data}
		if msg := preflightUnknownColumns(ctx.DB, physicalName, oneRow); msg != "" {
			return tools.Fail("%s", msg), nil
		}
		if msg := preflightRequiredColumns(ctx.DB, physicalName, oneRow); msg != "" {
			return tools.Fail("%s", msg), nil
		}
		if msg := preflightForeignKeys(ctx.DB, physicalName, oneRow); msg != "" {
			return tools.Fail("%s", msg), nil
		}
	}

	var colNames []string
	var placeholders []string
	var values []any

	for col, val := range data {
		if !store.ValidColumnName.MatchString(col) {
			return tools.Fail("invalid column name: %s", col), nil
		}
		colNames = append(colNames, col)
		placeholders = append(placeholders, "?")

		if secureCols != nil {
			if kind, isSecure := secureCols[col]; isSecure {
				processed, err := store.ProcessSecureValue(kind, val, ctx.Encryptor)
				if err != nil {
					return nil, fmt.Errorf("processing secure column %s: %w", col, err)
				}
				val = processed
			}
		}
		values = append(values, val)
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		physicalName, strings.Join(colNames, ", "), strings.Join(placeholders, ", "))

	res, err := txExec(ctx, tx).Exec(query, values...)
	if err != nil {
		return nil, fmt.Errorf("inserting row: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading last insert id: %w", err)
	}
	return tools.Ok(map[string]any{
		"id":    id,
		"table": tableName,
	}), nil
}

const bulkInsertCap = 100

func DataInsertBulk(ctx *tools.ToolContext, tx *sql.Tx, physicalName, tableName string, rowsRaw []any, secureCols map[string]string) (*tools.Result, error) {
	// Auto-chunk large standalone inserts into <=100-row batches instead of
	// rejecting them. The build agent hits the cap constantly while seeding
	// content and used to burn turns manually re-splitting the array (the
	// "insert failed 3x in a row" churn). Each batch commits in its own
	// transaction on the standalone path, which also localises a failure to a
	// single batch - exactly the "per-table inserts let the rows that succeeded
	// stand" property the build stage already relies on. Inside an outer
	// transaction we cannot open independent batches, so the original cap
	// still applies there.
	if len(rowsRaw) > bulkInsertCap {
		if tx != nil {
			return tools.Fail("maximum %d rows per bulk insert inside a transaction; split into <=%d-row batches", bulkInsertCap, bulkInsertCap), nil
		}
		return dataInsertChunked(ctx, physicalName, tableName, rowsRaw, secureCols)
	}

	// Pre-flight: catch a row missing required (NOT NULL, no-default) columns
	// BEFORE touching the DB, reporting every offending row at once (a raw
	// constraint error stops at the first). This is the common truncated-row
	// failure: a weak model emits a malformed first row (partial `body`, no
	// `title`/`slug`) and the rest complete, so the batch dies on row 0 with a
	// bare "NOT NULL constraint failed". The richer error names the missing
	// columns per row and flags rows that look truncated.
	//
	// Skipped inside an outer transaction: ctx.DB has MaxOpenConns=1, so
	// opening a PRAGMA cursor while the dispatched tx holds the only connection
	// deadlocks. Standalone bulk inserts - including every seed - own their
	// connection here and are checked.
	if tx == nil {
		if msg := preflightUnknownColumns(ctx.DB, physicalName, rowsRaw); msg != "" {
			return tools.Fail("%s", msg), nil
		}
		if msg := preflightRequiredColumns(ctx.DB, physicalName, rowsRaw); msg != "" {
			return tools.Fail("%s", msg), nil
		}
		if msg := preflightForeignKeys(ctx.DB, physicalName, rowsRaw); msg != "" {
			return tools.Fail("%s", msg), nil
		}
	}

	// When dispatched from a transaction, reuse the outer tx (SQLite doesn't
	// allow nesting). Single-call usage opens its own short-lived tx as before.
	var exec sqlExec
	var owned *sql.Tx
	if tx != nil {
		exec = tx
	} else {
		t, err := ctx.DB.Begin()
		if err != nil {
			return nil, fmt.Errorf("starting transaction: %w", err)
		}
		owned = t
		exec = t
		defer t.Rollback()
	}

	var insertedIDs []int64

	for i, rowRaw := range rowsRaw {
		row, ok := rowRaw.(map[string]any)
		if !ok {
			return tools.Fail("row %d is not a valid object", i), nil
		}

		var colNames []string
		var placeholders []string
		var values []any

		for col, val := range row {
			if !store.ValidColumnName.MatchString(col) {
				return tools.Fail("invalid column name in row %d: %s", i, col), nil
			}
			colNames = append(colNames, col)
			placeholders = append(placeholders, "?")

			if secureCols != nil {
				if kind, isSecure := secureCols[col]; isSecure {
					processed, err := store.ProcessSecureValue(kind, val, ctx.Encryptor)
					if err != nil {
						return nil, fmt.Errorf("processing secure column %s in row %d: %w", col, i, err)
					}
					val = processed
				}
			}
			values = append(values, val)
		}

		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			physicalName, strings.Join(colNames, ", "), strings.Join(placeholders, ", "))

		res, err := exec.Exec(query, values...)
		if err != nil {
			return nil, fmt.Errorf("inserting row %d: %w", i, err)
		}

		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("reading last insert id for row %d: %w", i, err)
		}
		insertedIDs = append(insertedIDs, id)
	}

	if owned != nil {
		if err := owned.Commit(); err != nil {
			return nil, fmt.Errorf("committing bulk insert: %w", err)
		}
	}

	return tools.Ok(map[string]any{
		"table":    tableName,
		"inserted": len(insertedIDs),
		"first_id": insertedIDs[0],
		"last_id":  insertedIDs[len(insertedIDs)-1],
	}), nil
}

// dataInsertChunked splits an over-cap standalone bulk insert into <=bulkInsertCap
// batches and runs each through DataInsertBulk (which re-preflights and commits
// that batch in its own transaction). Counts are aggregated so the caller sees
// one total. A failing batch stops the run and reports how many rows already
// committed, so the model retries only the tail instead of re-sending the whole
// array. Only reached on the standalone path (an outer tx is rejected upstream).
func dataInsertChunked(ctx *tools.ToolContext, physicalName, tableName string, rowsRaw []any, secureCols map[string]string) (*tools.Result, error) {
	total := 0
	batches := 0
	var firstID, lastID int64
	firstSet := false
	for start := 0; start < len(rowsRaw); start += bulkInsertCap {
		end := start + bulkInsertCap
		if end > len(rowsRaw) {
			end = len(rowsRaw)
		}
		res, err := DataInsertBulk(ctx, nil, physicalName, tableName, rowsRaw[start:end], secureCols)
		if err != nil {
			return nil, fmt.Errorf("bulk insert batch starting at row %d (after %d rows committed): %w", start, total, err)
		}
		if res != nil && !res.Success {
			// Validation failure (e.g. preflight NOT NULL / FK) for this batch:
			// surface it, prefixed with how far we got so the model fixes and
			// resubmits only the remaining rows.
			res.Error = fmt.Sprintf("%d rows already inserted; batch starting at row %d failed: %s", total, start, res.Error)
			return res, nil
		}
		batches++
		if d, ok := res.Data.(map[string]any); ok {
			if n, ok := d["inserted"].(int); ok {
				total += n
			}
			if fid, ok := d["first_id"].(int64); ok && !firstSet {
				firstID, firstSet = fid, true
			}
			if lid, ok := d["last_id"].(int64); ok {
				lastID = lid
			}
		}
	}
	return tools.Ok(map[string]any{
		"table":    tableName,
		"inserted": total,
		"first_id": firstID,
		"last_id":  lastID,
		"batches":  batches,
		"note":     fmt.Sprintf("input exceeded the %d-row bulk cap; auto-split into %d batches, each committed independently", bulkInsertCap, batches),
	}), nil
}

// DataUpsert inserts or updates on conflict. tx is the outer transaction when
// dispatched from manage_sql's transaction action, nil on a standalone call.
func DataUpsert(ctx *tools.ToolContext, a *DataActUpsert, tx *sql.Tx) (*tools.Result, error) {
	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	data := a.Data
	if len(data) == 0 {
		return tools.Fail("data is required for upsert"), nil
	}

	conflictCols := a.ConflictColumns
	if len(conflictCols) == 0 {
		return tools.Fail("conflict_columns is required for upsert"), nil
	}

	// Validate conflict columns: every name must be a real identifier, and never
	// `id` - the row's id is assigned by the table, so an upsert keyed on it can
	// never conflict and always inserts. Silently dropping a bad name used to turn
	// the call into a plain insert with no word of why.
	var conflictNames []string
	for _, c := range conflictCols {
		s, ok := c.(string)
		if !ok || !store.ValidColumnName.MatchString(s) {
			return tools.FailCode(tools.CodeBadArg, "conflict_columns entry %v is not a column name", c), nil
		}
		if s == "id" {
			return tools.FailCode(tools.CodeBadArg, "conflict_columns cannot be id: the id is assigned by the table, so an upsert on it always inserts. Use the natural key column (a UNIQUE slug, email, sku), or manage_data update with id to change a known row."), nil
		}
		conflictNames = append(conflictNames, s)
	}
	if len(conflictNames) == 0 {
		return tools.Fail("invalid conflict_columns"), nil
	}
	if r := checkRowValidation(ctx, tx, a.TableName, []map[string]any{data}); r != nil {
		return r, nil
	}

	// Process secure values.
	secureCols, _ := loadSecureColumnsTx(ctx, tx, a.TableName)
	for col, kind := range secureCols {
		if val, ok := data[col]; ok {
			processed, err := store.ProcessSecureValue(kind, val, ctx.Encryptor)
			if err != nil {
				return tools.Fail("processing %s: %v", col, err), nil
			}
			data[col] = processed
		}
	}

	var cols, placeholders, updateClauses []string
	var vals []any
	for col, val := range data {
		if !store.ValidColumnName.MatchString(col) || col == "id" {
			continue
		}
		cols = append(cols, col)
		placeholders = append(placeholders, "?")
		vals = append(vals, val)
		// Don't update conflict columns themselves.
		isConflict := false
		for _, cc := range conflictNames {
			if cc == col {
				isConflict = true
				break
			}
		}
		if !isConflict {
			updateClauses = append(updateClauses, fmt.Sprintf("%s = excluded.%s", col, col))
		}
	}

	exec := txExec(ctx, tx)
	if len(updateClauses) == 0 {
		// All columns are conflict columns, just do INSERT OR IGNORE.
		query := fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)",
			physicalName, strings.Join(cols, ","), strings.Join(placeholders, ","))
		_, err := exec.Exec(query, vals...)
		if err != nil {
			return tools.Fail("upsert error: %v", err), nil
		}
		return upsertResult(txQuerier(ctx, tx), physicalName, a.TableName, conflictNames, data)
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s",
		physicalName,
		strings.Join(cols, ","),
		strings.Join(placeholders, ","),
		strings.Join(conflictNames, ","),
		strings.Join(updateClauses, ","),
	)

	if _, err := exec.Exec(query, vals...); err != nil {
		return tools.Fail("upsert error: %v", err), nil
	}
	return upsertResult(txQuerier(ctx, tx), physicalName, a.TableName, conflictNames, data)
}

// upsertResult reads back the id of the row an upsert landed on by its conflict key.
// LastInsertId is wrong on the DO UPDATE path (it reports the last INSERT on the
// connection, not the updated row), and the insert-or-ignore path has none at all.
func upsertResult(q rowQuerier, physicalName, tableName string, conflictNames []string, data map[string]any) (*tools.Result, error) {
	where := make([]string, 0, len(conflictNames))
	args := make([]any, 0, len(conflictNames))
	for _, c := range conflictNames {
		where = append(where, c+" = ?")
		args = append(args, data[c])
	}
	out := map[string]any{"table": tableName, "upserted": true}
	var id int64
	if err := q.QueryRow(fmt.Sprintf("SELECT id FROM %s WHERE %s LIMIT 1", physicalName, strings.Join(where, " AND ")), args...).Scan(&id); err == nil {
		out["id"] = id
	}
	return tools.Ok(out), nil
}

// rowQuerier reads one row through a pool or an open transaction.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// txQuerier is txExec's read twin: the open transaction (so the read sees its own
// uncommitted write and does not wait on the writer pool it holds), else the DB.
func txQuerier(ctx *tools.ToolContext, tx *sql.Tx) rowQuerier {
	if tx != nil {
		return tx
	}
	return ctx.DB
}
