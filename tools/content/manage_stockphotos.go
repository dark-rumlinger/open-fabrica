package content

import "openfabrica/tools"

// manage_stockphotos is the Pexels/Unsplash/Pixabay stock-photo surface.
// Gated by the "stock" capability so the LLM only sees it when at least one
// provider is configured.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_stockphotos",
		Category: "content",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult:          8000,
		RequiresCapability: "stock",
		OneLiner:           "Search and fetch stock photos via Pexels, Unsplash, or Pixabay.",
		WhenToUse: `Use search to preview candidates before committing. Use fetch to download
N images by query and save them under /files (kind=file, scope=global)
so pages can reference them by path.`,
		Actions: []tools.Action{
			&StockSearch{}, &StockFetch{},
		},
	})
}

type StockSearch struct {
	Query    string `arg:"query,required" desc:"Search query."`
	Provider string `arg:"provider"       desc:"Provider name; omit for default."`
	Count    int    `arg:"count"          desc:"Max results (1-20)." default:"6" min:"1" max:"20"`
}

func (a *StockSearch) ActionName() string { return "search" }
func (a *StockSearch) Doc() string        { return "Return stock photo URLs matching a query." }
func (a *StockSearch) ReadOnly() bool     { return true }
func (a *StockSearch) Example() any {
	return &StockSearch{Query: "sourdough bakery"}
}
func (a *StockSearch) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return stockPhotosSearch(ctx, a.Query, a.Provider, a.Count)
}

type StockFetch struct {
	Query     string `arg:"query,required" desc:"Search query."`
	Count     int    `arg:"count"          desc:"Number of images to download (1-20)." default:"3" min:"1" max:"20"`
	PhotoSize string `arg:"photo_size"     desc:"Provider size hint (small/medium/large)." enum:"small,medium,large" default:"large"`
	Provider  string `arg:"provider"       desc:"Provider name; omit for default."`
}

func (a *StockFetch) ActionName() string { return "fetch" }
func (a *StockFetch) Doc() string        { return "Download stock photos and save them as artifacts." }
func (a *StockFetch) Example() any {
	return &StockFetch{Query: "sourdough bakery", Count: 3, PhotoSize: "large"}
}
func (a *StockFetch) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return stockPhotosFetch(ctx, a.Query, a.Count, a.PhotoSize, a.Provider)
}
