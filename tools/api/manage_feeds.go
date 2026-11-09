package api

import (
	"fmt"

	"openfabrica/tools"
)

// manage_feeds registers RSS / Atom feed endpoints served at /feed/<path>.
// Feed bytes are emitted by server/public/feeds.go on request.
//
// One arg struct per action. Create and update share most of the shape;
// mergeFeedOpts centralises the optional-fields map shape for both.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_feeds",
		Category: "api",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Define RSS and Atom feed endpoints served at /feed/<path> from a table.",
		WhenToUse: `Use for blogs, podcasts, news. format='rss' is the safe default; 'atom' if the
plan asks. update changes only the options you pass to a NEW non-empty value; an
omitted OR empty option keeps its stored value (a field cannot be blanked through
update - recreate with action=create to clear one). Link the feed from <head> for
reader auto-discovery.`,
		Actions: []tools.Action{
			&FeedCreate{},
			&FeedUpdate{},
			&FeedList{},
			&FeedDelete{},
		},
		Hints: func(action string, args map[string]any, r *tools.Result) string {
			if action == "create" && r != nil && r.Success {
				p, _ := args["endpoint_path"].(string)
				p = tools.NormalizeEndpointPath(p)
				mime := "application/rss+xml"
				if f, _ := args["format"].(string); f == "atom" {
					mime = "application/atom+xml"
				}
				return fmt.Sprintf("Feed ready at /feed/%s. Link from layout <head> with <link rel=\"alternate\" type=\"%s\" href=\"/feed/%s\">.", p, mime, p)
			}
			return ""
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type FeedCreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Suffix served at /feed/<path>."`
	TableName    string `arg:"table_name,required"    desc:"Existing table that holds the feed items."`
	Format       string `arg:"format"                 desc:"Feed format." enum:"rss,atom" default:"rss"`
	Title        string `arg:"title"                  desc:"Feed title."`
	Description  string `arg:"description"            desc:"Feed description."`
	Link         string `arg:"link"                   desc:"Canonical site URL." default:"/"`
	TitleColumn  string `arg:"title_column"           desc:"Column for item title (default title)."`
	BodyColumn   string `arg:"body_column"            desc:"Column for item body (default body)."`
	LinkColumn   string `arg:"link_column"            desc:"Optional per-item link column."`
	DateColumn   string `arg:"date_column"            desc:"Column with item date (default created_at)."`
	ItemLimit    int    `arg:"item_limit"             desc:"Max items in the feed (default 50)."`
	Where        []any  `arg:"where"                  desc:"Optional filter in the manage_data filters shape: [{column, op, value}]."`
}

func (a *FeedCreate) Doc() string { return "Register an RSS/Atom feed endpoint." }
func (a *FeedCreate) Example() any {
	return &FeedCreate{
		EndpointPath: "blog", TableName: "posts", Format: "rss", Title: "Blog",
		TitleColumn: "title", BodyColumn: "body", DateColumn: "published_at",
	}
}
func (a *FeedCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return feedCreate(ctx, a)
}

// --- update -----------------------------------------------------------------

type FeedUpdate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Feed endpoint path to patch."`
	TableName    string `arg:"table_name"             desc:"Change the backing table; omit to keep the stored table."`
	Format       string `arg:"format"                 desc:"Change the feed format; omit to keep the stored one." enum:"rss,atom"`
	Title        string `arg:"title"                  desc:"Change the feed title; omit to keep it."`
	Description  string `arg:"description"            desc:"Change the feed description; omit to keep it."`
	Link         string `arg:"link"                   desc:"Change the canonical site URL; omit to keep it."`
	TitleColumn  string `arg:"title_column"           desc:"Change the item-title column; omit to keep it."`
	BodyColumn   string `arg:"body_column"            desc:"Change the item-body column; omit to keep it."`
	LinkColumn   string `arg:"link_column"            desc:"Change the per-item link column; omit to keep it."`
	DateColumn   string `arg:"date_column"            desc:"Change the item-date column; omit to keep it."`
	ItemLimit    int    `arg:"item_limit"             desc:"Change the max item count; omit to keep it."`
	Where        []any  `arg:"where"                  desc:"Replace the WHERE filter; omit to keep the stored filter."`
}

func (a *FeedUpdate) Doc() string {
	return "Patch a feed endpoint: only the options you pass to a new non-empty value change; an omitted or empty option keeps its stored value (a field cannot be blanked here - recreate to clear it). Fails if no feed owns the path (create it first)."
}
func (a *FeedUpdate) Example() any {
	return &FeedUpdate{EndpointPath: "blog", Title: "My Blog", ItemLimit: 20}
}
func (a *FeedUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return feedUpdate(ctx, a)
}

// --- list -------------------------------------------------------------------

type FeedList struct{}

func (a *FeedList) Doc() string    { return "List feed endpoints." }
func (a *FeedList) ReadOnly() bool { return true }
func (a *FeedList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return feedList(ctx)
}

// --- delete -----------------------------------------------------------------

type FeedDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Feed endpoint path to delete."`
}

func (a *FeedDelete) Doc() string { return "Delete a feed endpoint." }
func (a *FeedDelete) Example() any {
	return &FeedDelete{EndpointPath: "blog"}
}
func (a *FeedDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return feedDelete(ctx, a.EndpointPath)
}
