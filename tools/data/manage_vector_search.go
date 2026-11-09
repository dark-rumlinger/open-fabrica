package data

import (
	"strings"

	"openfabrica/tools"
)

// manage_vector_search owns the semantic/vector path: requires an embeddings
// provider. The keyword-only FTS path lives in manage_keyword_search so the embedding
// capability gate hides only this tool from the LLM.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_vector_search",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult:          8000,
		Category:           "data",
		RequiresCapability: "embeddings",
		OneLiner:           "Search a project table with vector or hybrid (RRF) embeddings.",
		WhenToUse: `Use to find rows of a project table by meaning, not just keywords (e.g. "the
complaints about late delivery" in a feedback table). Requires an embedding
provider; without one this tool fails at runtime. Sequence: create_vector_index
(names the text columns) -> embed (fills vectors for existing rows; without it
semantic_search returns nothing) -> semantic_search, or hybrid_search, which fuses
keyword and vector rankings (RRF) and is preferred when an FTS index also exists.
To keep NEW rows embedded, add a manage_triggers ai_embed trigger on data.insert /
data.update with config {}. Only you can run these searches (the trigger just
keeps vectors current): the app's visitors have no semantic-search endpoint, so give
them keyword search (manage_data action=search) or a manage_knowledge chat endpoint instead.`,
		Actions: []tools.Action{
			&SearchCreateVectorIndex{},
			&SearchEmbed{},
			&SearchSemantic{},
			&SearchHybrid{},
		},
		Hints: func(action string, _ map[string]any, result *tools.Result) string {
			if result != nil && !result.Success && strings.Contains(result.Error, "no embedding provider") {
				return "Vector search needs an embeddings model on the install's platform LLM providers - an admin setting you cannot change from here, so do not retry. Use keyword search instead (manage_keyword_search create_index, then manage_data action=search)."
			}
			switch action {
			case "create_vector_index":
				return "Sequence: create_vector_index -> embed (populate vectors) -> semantic_search or hybrid_search. Without the embed step semantic_search returns 0 rows."
			case "embed":
				return "embed is a one-off pass over the current rows. To keep vectors current as rows change, add an ai_embed trigger (manage_triggers) instead of re-running embed."
			case "hybrid_search":
				return "Hybrid needs both an FTS index (manage_keyword_search create_index) and a vector index. Missing either degrades to single-mode ranking."
			}
			return ""
		},
	})
}

// SearchCreateVectorIndex builds an embedding index over named TEXT columns.
type SearchCreateVectorIndex struct {
	TableName        string `arg:"table_name,required"        desc:"Dynamic table to index."`
	EmbeddingColumns []any  `arg:"embedding_columns,required" desc:"TEXT columns to concatenate for embedding generation." items:"string"`
}

func (a *SearchCreateVectorIndex) ActionName() string { return "create_vector_index" }
func (a *SearchCreateVectorIndex) Doc() string {
	return "Build an embedding index over the named TEXT columns."
}
func (a *SearchCreateVectorIndex) Example() any {
	return &SearchCreateVectorIndex{TableName: "articles", EmbeddingColumns: []any{"title", "body"}}
}
func (a *SearchCreateVectorIndex) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return searchCreateVectorIndex(ctx, a.TableName, a.EmbeddingColumns)
}

// SearchEmbed enqueues rows for embedding (empty row_ids = all rows).
type SearchEmbed struct {
	TableName string `arg:"table_name,required" desc:"Indexed table."`
	RowIDs    []any  `arg:"row_ids"             desc:"Specific row IDs to embed (empty = all rows)." items:"integer"`
}

func (a *SearchEmbed) ActionName() string { return "embed" }
func (a *SearchEmbed) Doc() string {
	return "Embed rows now (empty row_ids = all rows); a one-off pass, not kept current as rows change."
}
func (a *SearchEmbed) Example() any { return &SearchEmbed{TableName: "articles"} }
func (a *SearchEmbed) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return searchEmbed(ctx, a.TableName, a.RowIDs)
}

// SearchSemantic is pure vector-similarity search by natural language query.
type SearchSemantic struct {
	TableName     string  `arg:"table_name,required"     desc:"Indexed table."`
	SemanticQuery string  `arg:"semantic_query,required" desc:"Natural language query."`
	TopK          int     `arg:"top_k"                   desc:"Number of results." default:"10"`
	Threshold     float64 `arg:"threshold"               desc:"Minimum cosine similarity 0-1. Relevant text scores about 0.3-0.6 on real embedding models, so 0.5 returns nothing; raise it only to be stricter." default:"0.25"`
}

func (a *SearchSemantic) ActionName() string { return "semantic_search" }
func (a *SearchSemantic) Doc() string {
	return "Pure vector similarity search by natural language query."
}
func (a *SearchSemantic) ReadOnly() bool { return true }
func (a *SearchSemantic) Example() any {
	return &SearchSemantic{TableName: "articles", SemanticQuery: "how to start sourdough"}
}
func (a *SearchSemantic) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return searchSemantic(ctx, a.TableName, a.SemanticQuery, a.TopK, a.Threshold)
}

// SearchHybrid fuses keyword + vector ranking via Reciprocal Rank Fusion.
type SearchHybrid struct {
	TableName     string `arg:"table_name,required"     desc:"Indexed table."`
	SemanticQuery string `arg:"semantic_query,required" desc:"Natural language query."`
	TopK          int    `arg:"top_k"                   desc:"Number of results." default:"10"`
}

func (a *SearchHybrid) ActionName() string { return "hybrid_search" }
func (a *SearchHybrid) Doc() string        { return "RRF-fused keyword + vector ranking." }
func (a *SearchHybrid) ReadOnly() bool     { return true }
func (a *SearchHybrid) Example() any {
	return &SearchHybrid{TableName: "articles", SemanticQuery: "sourdough starter"}
}
func (a *SearchHybrid) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return searchHybrid(ctx, a.TableName, a.SemanticQuery, a.TopK)
}
