package data

// DataTransaction implements manage_sql action="transaction", an ordered list of
// mutations that commit or roll back together. Closes the "debit A, credit B, or
// fail both" gap single-row CRUD can't satisfy: transfers, ledger postings,
// order-with-inventory decrements - anything where a partial write leaves the
// table invalid.
//
// The handler opens one tx, decodes each inner-op map into the matching
// manage_data Action struct (tools.PopulateAction - the same tag-driven decode
// the tool loop uses), and dispatches through the typed mutation handlers
// (insert/update/delete/upsert/batch_delete) with the tx passed explicitly.
// Handlers route their executor through txExec, which falls back to ctx.DB on
// a nil tx, so single-call usage is unchanged. SQLite has no nested tx, so the
// allowed-op set excludes "transaction" and the bulk-insert path skips its own
// BeginTx when a tx is passed.

import (
	"database/sql"
	"fmt"

	"openfabrica/tools"
)

// transactionAllowedOps is the closed set of inner-op actions a transaction
// may dispatch. Read-only ops (query/count/aggregate/search) are excluded
// because there's no benefit to running a SELECT inside the same atomic unit
// , and including them would muddy the contract (a SELECT failure shouldn't
// roll back prior writes). Schema DDL is also excluded: SQLite implicitly
// commits before DDL on most operations and the transaction guarantee would
// be illusory.
var transactionAllowedOps = map[string]bool{
	"insert":       true,
	"update":       true,
	"delete":       true,
	"upsert":       true,
	"batch_delete": true,
}

func DataTransaction(ctx *tools.ToolContext, a *SQLTransaction) (*tools.Result, error) {
	opsRaw := a.Operations
	if len(opsRaw) == 0 {
		return tools.Fail("operations is required: an ordered list of {action, table_name, ...} entries"), nil
	}
	if len(opsRaw) > 100 {
		return tools.Fail("maximum 100 operations per transaction"), nil
	}

	tx, err := ctx.DB.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	results := make([]map[string]any, 0, len(opsRaw))

	for i, opRaw := range opsRaw {
		op, ok := opRaw.(map[string]any)
		if !ok {
			return tools.Fail("operation %d is not a valid object", i), nil
		}

		action, _ := op["action"].(string)
		if action == "" {
			return tools.Fail("operation %d: action is required (one of insert / update / delete / upsert / batch_delete)", i), nil
		}
		if !transactionAllowedOps[action] {
			return tools.Fail(
				"operation %d: action %q not allowed inside transaction, use one of insert / update / delete / upsert / batch_delete (read-only ops and DDL are not transactional in this context)",
				i, action,
			), nil
		}

		res, herr := dispatchTransactionOp(ctx, action, op, tx)
		if herr != nil {
			return tools.Fail("operation %d (%s on %v): %v", i, action, op["table_name"], herr), nil
		}
		if res == nil || !res.Success {
			msg := "unknown error"
			if res != nil {
				msg = res.Error
			}
			return tools.Fail("operation %d (%s on %v): %s", i, action, op["table_name"], msg), nil
		}

		summary := map[string]any{
			"action":     action,
			"table_name": op["table_name"],
		}
		if data, ok := res.Data.(map[string]any); ok {
			for _, k := range []string{"id", "rows_affected", "deleted", "inserted", "first_id", "last_id"} {
				if v, has := data[k]; has {
					summary[k] = v
				}
			}
		}
		results = append(results, summary)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return tools.Ok(map[string]any{
		"committed":  len(results),
		"operations": results,
	}), nil
}

// dispatchTransactionOp decodes one inner-op map into the matching manage_data
// Action struct and runs its typed mutation handler on the outer tx. Kept as a
// switch so the closed set stays visible alongside transactionAllowedOps,
// adding an action here without adding it there (or vice versa) would be a
// clear inconsistency on review.
func dispatchTransactionOp(ctx *tools.ToolContext, action string, op map[string]any, tx *sql.Tx) (*tools.Result, error) {
	populate := func(dst tools.Action) *tools.Result {
		if err := tools.PopulateAction(dst, op); err != nil {
			return tools.Fail("%s", err.Error())
		}
		return nil
	}
	switch action {
	case "insert":
		a := &DataActInsert{}
		if r := populate(a); r != nil {
			return r, nil
		}
		return DataInsert(ctx, a, tx)
	case "update":
		a := &DataActUpdate{}
		if r := populate(a); r != nil {
			return r, nil
		}
		return DataUpdate(ctx, a, tx)
	case "delete":
		a := &DataActDelete{}
		if r := populate(a); r != nil {
			return r, nil
		}
		return DataDelete(ctx, a, tx)
	case "upsert":
		a := &DataActUpsert{}
		if r := populate(a); r != nil {
			return r, nil
		}
		return DataUpsert(ctx, a, tx)
	case "batch_delete":
		a := &DataActBatchDelete{}
		if r := populate(a); r != nil {
			return r, nil
		}
		return DataBatchDelete(ctx, a, tx)
	}
	return nil, fmt.Errorf("unhandled transaction action %q", action)
}
