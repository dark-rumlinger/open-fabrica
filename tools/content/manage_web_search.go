package content

import "openfabrica/tools"

// manage_web_search gives the build/live model real-time web search via the
// configured search providers (Tavily, Brave). Gated by the "search"
// capability so the LLM only sees it when at least one provider is
// configured. Use it to ground decisions in current information: the
// latest version of a library, a package's current API, recent news, or
// anything that may have changed since the model's training cutoff.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_web_search",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult:          8000,
		Category:           "content",
		RequiresCapability: "search",
		OneLiner:           "Search the live web for current info (latest library versions, docs, news).",
		WhenToUse: `query looks up information newer than your training data: the current
stable version of a dependency, a library's present-day API, recent news/events.
Set topic="news" with a freshness window for time-sensitive lookups. Results are
titles, URLs, and content snippets (plus a synthesized answer when the backend
supports one); cite the URLs. Read-only, saves nothing. list_providers shows which
search backends are configured.`,
		Actions: []tools.Action{
			&WebSearch{}, &SearchListProviders{},
		},
	})
}

type WebSearch struct {
	Query     string `arg:"query,required" desc:"What to search for."`
	Count     int    `arg:"count"          desc:"Max results (1-20)." default:"6" min:"1" max:"20"`
	Topic     string `arg:"topic"          desc:"'news' for recent articles, 'general' for the open web." enum:"general,news" default:"general"`
	Freshness string `arg:"freshness"      desc:"Recency window for results." enum:"day,week,month,year"`
	Provider  string `arg:"provider"       desc:"Provider name; omit for the default."`
}

func (a *WebSearch) ActionName() string { return "query" }
func (a *WebSearch) Doc() string        { return "Search the live web and return matching results." }
func (a *WebSearch) ReadOnly() bool     { return true }
func (a *WebSearch) Example() any {
	return &WebSearch{Query: "latest stable version of htmx", Count: 5}
}
func (a *WebSearch) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webSearchQuery(ctx, a.Query, a.Count, a.Topic, a.Freshness, a.Provider)
}

type SearchListProviders struct{}

func (a *SearchListProviders) ActionName() string { return "list_providers" }
func (a *SearchListProviders) Doc() string        { return "List configured web-search providers." }
func (a *SearchListProviders) ReadOnly() bool     { return true }
func (a *SearchListProviders) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webSearchListProviders(ctx)
}
