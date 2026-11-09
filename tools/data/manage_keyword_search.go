package data

import (
	"openfabrica/tools"
)

// manage_keyword_search owns the FTS5 keyword path: pure SQLite, no external
// dependencies, always available. Vector / semantic / hybrid actions live
// in manage_vector_search so the embedding-provider gate hides only the
// vector tool from the LLM, not the keyword path.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_keyword_search",
		Category: "data",
		OneLiner: "Create and manage a table's FTS5 keyword index (BM25); query it with manage_data action=search.",
		WhenToUse: `Create and manage a table's keyword (FTS5) index for ranked full-text
search. The table needs searchable_columns declared at create_table time, or use
create_index here to add one after the fact; triggers keep the FTS table in sync.
Query the index with manage_data action=search; manage_vector_search adds
semantic/hybrid similarity (needs an embedding provider). list_indexes/drop_index
manage existing indexes.`,
		Actions: []tools.Action{
			&SearchCreateIndex{},
			&SearchDropIndex{},
			&SearchListIndexes{},
		},
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			if action == "create_index" && r != nil && r.Success {
				return `Query via manage_data action=search. Triggers auto-sync on INSERT/UPDATE/DELETE.`
			}
			return ""
		},
	})
}

// SearchCreateIndex builds an FTS5 index over named TEXT columns.
type SearchCreateIndex struct {
	TableName string `arg:"table_name,required" desc:"Dynamic table to index."`
	Columns   []any  `arg:"columns,required"    desc:"TEXT columns to include in the index." items:"string"`
}

func (a *SearchCreateIndex) ActionName() string { return "create_index" }
func (a *SearchCreateIndex) Doc() string {
	return "Build an FTS5 index over the named TEXT columns and install sync triggers."
}
func (a *SearchCreateIndex) Example() any {
	return &SearchCreateIndex{TableName: "articles", Columns: []any{"title", "body"}}
}
func (a *SearchCreateIndex) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return searchCreateIndex(ctx, a.TableName, a.Columns)
}

// SearchDropIndex removes an FTS index and its sync triggers.
type SearchDropIndex struct {
	TableName string `arg:"table_name,required" desc:"Indexed table."`
}

func (a *SearchDropIndex) ActionName() string { return "drop_index" }
func (a *SearchDropIndex) Doc() string        { return "Remove the FTS index and its sync triggers." }
func (a *SearchDropIndex) Example() any       { return &SearchDropIndex{TableName: "articles"} }
func (a *SearchDropIndex) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return searchDropIndex(ctx, a.TableName)
}

// SearchListIndexes lists every FTS index in this project.
type SearchListIndexes struct{}

func (a *SearchListIndexes) ActionName() string { return "list_indexes" }
func (a *SearchListIndexes) Doc() string        { return "List every FTS index in this project." }
func (a *SearchListIndexes) ReadOnly() bool     { return true }
func (a *SearchListIndexes) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return searchListIndexes(ctx)
}
