package content

import "openfabrica/tools"

// manage_web_fetch retrieves a known HTTP/HTTPS URL and extracts its readable
// content as clean Markdown/text. Pairs with manage_web_search: search finds
// candidate URLs, while manage_web_fetch reads the full page, docs, or feed.

func init() {
	tools.Define(tools.Tool{
		Name:      "manage_web_fetch",
		MaxResult: 24000,
		Category:  "content",
		OneLiner:  "Fetch a web page, doc, or feed URL and extract its readable content as Markdown/text.",
		WhenToUse: `fetch reads a known http/https URL (from manage_web_search, a user message,
or a scheduled task) and returns clean extracted text/Markdown. download saves a
binary asset (image, .glb/.gltf/.obj 3D model, .mp3/.wav/.ogg audio, .woff2 font)
directly into /files for the project. Prefer manage_web_search when you need to
discover URLs for a topic; use manage_web_fetch once you have a specific URL.
fetch is read-only and supports offset/max_chars pagination.`,
		Actions: []tools.Action{
			&WebFetch{},
			&WebDownload{},
		},
	})
}

type WebFetch struct {
	URL           string `arg:"url,required"   desc:"HTTP or HTTPS URL to fetch."`
	Offset        int    `arg:"offset"         desc:"Character offset into the extracted content." default:"0" min:"0"`
	MaxChars      int    `arg:"max_chars"      desc:"Maximum characters to return." default:"8000" min:"500" max:"20000"`
	ExtractDesign bool   `arg:"extract_design" desc:"Also extract navigation links, feeds, and visual design tokens (colors, fonts, CSS vars) from HTML." default:"false"`
}

func (a *WebFetch) ActionName() string { return "fetch" }
func (a *WebFetch) Doc() string        { return "Fetch a URL and extract its readable content." }
func (a *WebFetch) ReadOnly() bool     { return true }
func (a *WebFetch) Example() any {
	return &WebFetch{URL: "https://go.dev/doc/effective_go", Offset: 0, MaxChars: 8000}
}
func (a *WebFetch) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webFetchURL(ctx, a.URL, a.Offset, a.MaxChars, a.ExtractDesign)
}

type WebDownload struct {
	URL      string `arg:"url,required"      desc:"HTTP or HTTPS URL of the asset to download."`
	Filename string `arg:"filename,required" desc:"Destination path under /files (e.g. models/lantern.glb, audio/rain.mp3, hero.jpg)."`
}

func (a *WebDownload) ActionName() string { return "download" }
func (a *WebDownload) Doc() string {
	return "Download an allowed binary asset (image, 3D model, audio, font) into /files."
}
func (a *WebDownload) ReadOnly() bool { return false }
func (a *WebDownload) Example() any {
	return &WebDownload{URL: "https://example.com/assets/lantern.glb", Filename: "lantern.glb"}
}
func (a *WebDownload) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webDownloadAsset(ctx, a.URL, a.Filename)
}
