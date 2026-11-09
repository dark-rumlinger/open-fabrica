package data

import "openfabrica/tools"

// manage_knowledge builds a retrieval knowledge base: it chunks documents,
// embeds them, and answers similarity queries - the ingestion half of RAG. Bind
// a base to a chat endpoint with manage_llm_proxy create knowledge_base=<name>
// to get a retrieval-augmented assistant. Requires an embeddings provider.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_knowledge",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult:          8000,
		Category:           "data",
		RequiresCapability: "embeddings",
		OneLiner:           "Build a RAG knowledge base: chunk + embed documents, then bind it to an LLM chat endpoint.",
		WhenToUse: `Use to build "chat over my documents / support assistant / internal Q&A"
features. Sequence: create_base -> ingest documents -> bind it to a chat endpoint
with manage_llm_proxy action=create knowledge_base=<name>. The endpoint then
retrieves the most relevant chunks per question and answers from them.
Ingestion options: ingest_path points at a file OR folder on the server where
OpenFabrica runs and pulls in every text/code file (any UTF-8 text: .txt .md
.html .xml .csv .json .js .ts .py .yaml .sql .css ... ) plus .pdf and .docx
(PDFs are text-extracted, no OCR for scanned pages); binary files are skipped.
ingest takes inline text, or from_file: a project file you saved OR one the owner
uploaded in the Files tab (text, .pdf and .docx are extracted the same way). Requires an
embeddings provider (same gate as manage_vector_search). Use retrieve to inspect
what a query would surface.`,
		Actions: []tools.Action{
			&KnowledgeCreateBase{},
			&KnowledgeIngest{},
			&KnowledgeIngestPath{},
			&KnowledgeRetrieve{},
			&KnowledgeListBases{},
			&KnowledgeDeleteBase{},
		},
		Hints: func(action string, _ map[string]any, result *tools.Result) string {
			if result != nil && !result.Success {
				return ""
			}
			switch action {
			case "create_base":
				return "Now ingest documents into this base, then create the chat endpoint with manage_llm_proxy create knowledge_base=<name>."
			case "ingest":
				return "Re-running ingest adds more chunks; embeddings are generated automatically for the new chunks only."
			}
			return ""
		},
	})
}

// KnowledgeCreateBase creates a named knowledge base (chunk table + vector index).
type KnowledgeCreateBase struct {
	Name        string `arg:"name,required" desc:"Unique knowledge base name, e.g. \"handbook\"."`
	Description string `arg:"description"   desc:"What this base contains."`
}

func (a *KnowledgeCreateBase) Doc() string { return "Create a named knowledge base." }
func (a *KnowledgeCreateBase) Example() any {
	return &KnowledgeCreateBase{Name: "handbook", Description: "Employee handbook sections"}
}
func (a *KnowledgeCreateBase) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return knowledgeCreateBase(ctx, a.Name, a.Description)
}

// KnowledgeIngest chunks and embeds a document into a base.
type KnowledgeIngest struct {
	Base      string `arg:"base,required" desc:"Knowledge base name to ingest into."`
	Source    string `arg:"source"        desc:"Label identifying this document (shown with retrieved chunks)."`
	Text      string `arg:"text"          desc:"Document text to ingest. Provide this or from_file."`
	FromFile  string `arg:"from_file"     desc:"Path of a project file to ingest instead of inline text: one you saved or one the owner uploaded in the Files tab, e.g. \"handbook.pdf\". Text files, .pdf and .docx are extracted."`
	ChunkSize int    `arg:"chunk_size"    desc:"Characters per chunk." default:"1000"`
	Overlap   int    `arg:"overlap"       desc:"Characters of overlap between chunks." default:"150"`
}

func (a *KnowledgeIngest) Doc() string {
	return "Chunk a document, store the chunks, and embed them into a base."
}
func (a *KnowledgeIngest) Example() any {
	return &KnowledgeIngest{Base: "handbook", Source: "pto-policy", Text: "Employees accrue 2 days of PTO per month..."}
}
func (a *KnowledgeIngest) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return knowledgeIngest(ctx, a.Base, a.Source, a.Text, a.FromFile, a.ChunkSize, a.Overlap)
}

// KnowledgeIngestPath ingests a file or folder from the server's filesystem.
type KnowledgeIngestPath struct {
	Base      string `arg:"base,required" desc:"Knowledge base name to ingest into."`
	Path      string `arg:"path,required" desc:"Absolute path to a file or folder on the server where OpenFabrica runs. A folder ingests every supported document inside it."`
	Recursive bool   `arg:"recursive"     desc:"When path is a folder, also descend into subfolders."`
	ChunkSize int    `arg:"chunk_size"    desc:"Characters per chunk." default:"1000"`
	Overlap   int    `arg:"overlap"       desc:"Characters of overlap between chunks." default:"150"`
}

func (a *KnowledgeIngestPath) Doc() string {
	return "Ingest a file or whole folder from the server filesystem: any UTF-8 text/code file (.txt .md .html .xml .csv .json .js .py .yaml ...) plus .pdf and .docx. PDFs are text-extracted (no OCR); binary files are skipped. The owner approves each path first."
}
func (a *KnowledgeIngestPath) Example() any {
	return &KnowledgeIngestPath{Base: "handbook", Path: "/srv/docs/handbook", Recursive: true}
}
func (a *KnowledgeIngestPath) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return knowledgeIngestPath(ctx, a.Base, a.Path, a.Recursive, a.ChunkSize, a.Overlap)
}

// KnowledgeRetrieve returns the most relevant chunks for a query (inspection).
type KnowledgeRetrieve struct {
	Base  string `arg:"base,required"  desc:"Knowledge base name."`
	Query string `arg:"query,required" desc:"Natural-language query."`
	TopK  int    `arg:"top_k"          desc:"Number of chunks to return." default:"5"`
}

func (a *KnowledgeRetrieve) Doc() string {
	return "Return the most relevant chunks for a query (debug/inspection)."
}
func (a *KnowledgeRetrieve) ReadOnly() bool { return true }
func (a *KnowledgeRetrieve) Example() any {
	return &KnowledgeRetrieve{Base: "handbook", Query: "how much PTO do I get?"}
}
func (a *KnowledgeRetrieve) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return knowledgeRetrieve(ctx, a.Base, a.Query, a.TopK)
}

// KnowledgeListBases lists defined knowledge bases.
type KnowledgeListBases struct{}

func (a *KnowledgeListBases) Doc() string    { return "List knowledge bases and their chunk counts." }
func (a *KnowledgeListBases) ReadOnly() bool { return true }
func (a *KnowledgeListBases) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return knowledgeListBases(ctx)
}

// KnowledgeDeleteBase removes a base and its chunks.
type KnowledgeDeleteBase struct {
	Base string `arg:"base,required" desc:"Knowledge base name to delete."`
}

func (a *KnowledgeDeleteBase) Doc() string { return "Delete a knowledge base and all its chunks." }
func (a *KnowledgeDeleteBase) Example() any {
	return &KnowledgeDeleteBase{Base: "handbook"}
}
func (a *KnowledgeDeleteBase) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return knowledgeDeleteBase(ctx, a.Base)
}
