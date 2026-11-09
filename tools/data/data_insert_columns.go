package data

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/internal/dbh"
	"openfabrica/store"
)

// preflightUnknownColumns names the keys of the rows to insert that the table does not
// have, with a did-you-mean for each, before anything is written. A weak model seeding a
// 60-row array drops or doubles a few letters of a column name ("orderorder_id"); the
// driver then says only "table has no column named X" for the first bad row, and every fix
// costs a full resend of the array. Naming every bad key at once, with the closest real
// column, makes it one resend. Best effort: an introspection miss returns "" and the insert
// proceeds to the driver as before.
func preflightUnknownColumns(db dbh.Handle, physicalName string, rowsRaw []any) string {
	cols, err := store.GetTableColumnInfo(db, physicalName)
	if err != nil || len(cols) == 0 {
		return ""
	}
	have := make(map[string]bool, len(cols))
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		have[strings.ToLower(c.Name)] = true
		names = append(names, c.Name)
	}
	unknown := map[string][]int{} // key -> 1-based row numbers
	for i, rr := range rowsRaw {
		row, ok := rr.(map[string]any)
		if !ok {
			continue
		}
		for k := range row {
			if !have[strings.ToLower(k)] {
				unknown[k] = append(unknown[k], i+1)
			}
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	keys := make([]string, 0, len(unknown))
	for k := range unknown {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "nothing was inserted: the table has no column(s) %s. Its columns are: %s.", quoteAll(keys), strings.Join(names, ", "))
	for _, k := range keys {
		rows := unknown[k]
		where := fmt.Sprintf("row %d", rows[0])
		if len(rows) > 1 {
			where = fmt.Sprintf("%d rows, first row %d", len(rows), rows[0])
		}
		if s := store.SuggestName(k, names, 2); len(s) > 0 {
			fmt.Fprintf(&b, " %q (%s): did you mean %s?", k, where, strings.Join(s, " or "))
		} else {
			fmt.Fprintf(&b, " %q (%s) matches no column.", k, where)
		}
	}
	b.WriteString(" Fix those keys and resend; if the column belongs on the table, add it with manage_schema action=alter_table first.")
	return b.String()
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}
