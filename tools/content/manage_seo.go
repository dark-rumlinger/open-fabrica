package content

import (
	"openfabrica/tools"
	"openfabrica/tools/project"
)

// manage_seo manages per-page meta tags, sitemap, robots.txt, and a
// validation audit. All actions are gated by project.RequirePagesFirst so
// they error out on headless projects.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_seo",
		Category: "content",
		OneLiner: "Manage per-page SEO meta tags, sitemap, and robots.txt (with validation).",
		WhenToUse: `Use set_meta after creating a page to attach title/description/og tags.
/sitemap.xml is served live from the published pages, so generate_sitemap only
reports what it lists. validate runs
a per-page audit of missing SEO fields - run it once before the
project goes live.`,
		Actions: []tools.Action{
			&SEOSetMeta{}, &SEOGetMeta{}, &SEOGenerateSitemap{},
			&SEOSetRobots{}, &SEOValidate{},
		},
	})
}

type SEOSetMeta struct {
	PagePath    string         `arg:"page_path,required" desc:"Page to attach meta tags to."`
	Title       string         `arg:"title"              desc:"Page <title>; overrides the title in the page's own metadata."`
	Description string         `arg:"description"        desc:"Meta description."`
	OG          map[string]any `arg:"og"                 desc:"OpenGraph fields, keyed title/description/image (e.g. {\"title\":\"...\",\"image\":\"https://...\"})."`
	Canonical   string         `arg:"canonical"          desc:"Canonical URL."`
	// The renderer already emits all three (render.go loadSEOMeta); they had no arg,
	// so JSON-LD and og:locale were unreachable through any tool.
	StructuredData string `arg:"structured_data" desc:"JSON-LD emitted as <script type=\"application/ld+json\">. Pass the JSON object as a string, e.g. an Article/Product/Recipe/Event schema.org node - this is what earns a rich result in search."`
	TwitterCard    string `arg:"twitter_card"    desc:"Twitter card type: summary_large_image for a hero image, summary otherwise." enum:"summary,summary_large_image"`
	Locale         string `arg:"locale"          desc:"og:locale (e.g. \"en_US\", \"ar_EG\"); sets <html lang>/direction only on a page with no layout."`
}

func (a *SEOSetMeta) Doc() string {
	return "Set or update per-page meta tags. Every field is replaced by what you pass, so send the full set you want on the page, not just the one you are changing."
}
func (a *SEOSetMeta) Example() any {
	return &SEOSetMeta{PagePath: "/", Title: "Home", Description: "Welcome"}
}
func (a *SEOSetMeta) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	// The handler stores flat og_title/og_description/og_image and canonical_url
	// columns; unpack the og map and rename canonical so the values actually land.
	var ogTitle, ogDesc, ogImage string
	if a.OG != nil {
		ogTitle, _ = a.OG["title"].(string)
		ogDesc, _ = a.OG["description"].(string)
		ogImage, _ = a.OG["image"].(string)
	}
	return project.RequirePagesFirst("set_meta", func(ctx *tools.ToolContext, _ map[string]any) (*tools.Result, error) {
		return seoSetMeta(ctx, seoMetaSpec{
			PagePath: a.PagePath, Title: a.Title, Description: a.Description,
			OGTitle: ogTitle, OGDescription: ogDesc, OGImage: ogImage,
			Canonical: a.Canonical, Structured: a.StructuredData,
			TwitterCard: a.TwitterCard, Locale: a.Locale,
		})
	})(ctx, nil)
}

type SEOGetMeta struct {
	PagePath string `arg:"page_path,required" desc:"Page to read meta from."`
}

func (a *SEOGetMeta) Doc() string    { return "Read per-page meta tags." }
func (a *SEOGetMeta) ReadOnly() bool { return true }
func (a *SEOGetMeta) Example() any   { return &SEOGetMeta{PagePath: "/"} }
func (a *SEOGetMeta) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return project.RequirePagesFirst("get_meta", func(ctx *tools.ToolContext, _ map[string]any) (*tools.Result, error) {
		return seoGetMeta(ctx, a.PagePath)
	})(ctx, nil)
}

type SEOGenerateSitemap struct{}

func (a *SEOGenerateSitemap) Doc() string {
	return "List the pages /sitemap.xml serves (it is generated live; nothing is written)."
}
func (a *SEOGenerateSitemap) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return project.RequirePagesFirst("generate_sitemap", func(ctx *tools.ToolContext, _ map[string]any) (*tools.Result, error) {
		return seoGenerateSitemap(ctx)
	})(ctx, nil)
}

type SEOSetRobots struct {
	Content string `arg:"content,required" desc:"robots.txt body."`
}

func (a *SEOSetRobots) Doc() string { return "Write /robots.txt content." }
func (a *SEOSetRobots) Example() any {
	return &SEOSetRobots{Content: "User-agent: *\nAllow: /"}
}
func (a *SEOSetRobots) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return project.RequirePagesFirst("set_robots", func(ctx *tools.ToolContext, _ map[string]any) (*tools.Result, error) {
		return seoSetRobots(ctx, a.Content)
	})(ctx, nil)
}

type SEOValidate struct{}

func (a *SEOValidate) Doc() string {
	return "Per-page audit of missing SEO fields (title, description, Open Graph)."
}
func (a *SEOValidate) ReadOnly() bool { return true }
func (a *SEOValidate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return project.RequirePagesFirst("validate", func(ctx *tools.ToolContext, _ map[string]any) (*tools.Result, error) {
		return seoValidate(ctx)
	})(ctx, nil)
}
