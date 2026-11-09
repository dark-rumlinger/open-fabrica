package content

import (
	"time"

	"openfabrica/tools"
)

// manage_documents renders a project page or an inline HTML string to PDF and
// saves it as a kind=file artifact served at /files/<name>. Backed by the same
// headless-Chrome launcher the runtime probe uses, so it needs no external
// provider, only a Chrome/Edge binary on the host. Unlocks invoices, receipts,
// reports, certificates, tickets, contracts, and any other printable artifact.

func init() {
	tools.Define(tools.Tool{
		Name:        "manage_documents",
		Category:    "content",
		OneLiner:    "Render a project page or inline HTML to PDF, saved as a kind=file at /files/<name>.",
		MaxDuration: 60 * time.Second,
		WhenToUse: `Use when the project must produce a downloadable document: an invoice, receipt,
report, certificate, ticket, or contract. Prefer render_url to capture an
existing styled page (its CSS/assets resolve correctly) - build the page once,
then render any instance like /invoices/5 to PDF. Use render_html for a one-off
document not backed by a page (inline or absolute styles only; relative asset
URLs do not resolve). Output lands at /files/<name> and is linked like any file.
render_url and render_html make ONE document while building. For a document made
per request at runtime (an invoice for order 17, a ticket for a booking) use
create_endpoint: it stores an HTML template and serves POST /api/<path>/pdf, which
fills it from the request's JSON data and returns the PDF.
Requires a Chrome/Edge binary on the host; without one the tool returns a
browser_unavailable error.`,
		Actions: []tools.Action{
			&DocumentRenderURL{},
			&DocumentRenderHTML{},
			&DocumentList{},
			&DocumentCreateEndpoint{},
			&DocumentListEndpoints{},
		},
	})
}

// DocumentRenderURL renders an existing project route to PDF.
type DocumentRenderURL struct {
	PageRoute       string  `arg:"page_route,required" desc:"Project PAGE route to render, e.g. \"/invoices/5\" - a page route, never a /files/... path. A route that answers 404 is refused."`
	Filename        string  `arg:"filename"          desc:"Output filename (\".pdf\" appended if absent). Omit for a slug of the path, unlike the other actions, which default to \"document\"."`
	PageSize        string  `arg:"page_size"         desc:"Paper size." enum:"A4,Letter,Legal" default:"A4"`
	Landscape       bool    `arg:"landscape"         desc:"Render in landscape orientation."`
	PrintBackground bool    `arg:"print_background"  desc:"Include CSS background colours/images." default:"true"`
	MarginInches    float64 `arg:"margin_inches"     desc:"Uniform page margin in inches (default 0.4)."`
}

func (a *DocumentRenderURL) ActionName() string { return "render_url" }
func (a *DocumentRenderURL) Doc() string {
	return "Render an existing project page/route to a PDF document."
}
func (a *DocumentRenderURL) Example() any {
	return &DocumentRenderURL{PageRoute: "/invoices/5", Filename: "invoice-5", PageSize: "A4", PrintBackground: true}
}
func (a *DocumentRenderURL) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return documentRenderURL(ctx, a.PageRoute, a.Filename, a.PageSize, a.Landscape, a.PrintBackground, a.MarginInches)
}

// DocumentRenderHTML renders an inline HTML string to PDF.
type DocumentRenderHTML struct {
	HTML            string  `arg:"html,required"     desc:"Full HTML document to render. Inline or absolute styles only; relative asset URLs do not resolve."`
	Filename        string  `arg:"filename"          desc:"Output filename (\".pdf\" appended if absent)." default:"document"`
	PageSize        string  `arg:"page_size"         desc:"Paper size." enum:"A4,Letter,Legal" default:"A4"`
	Landscape       bool    `arg:"landscape"         desc:"Render in landscape orientation."`
	PrintBackground bool    `arg:"print_background"  desc:"Include CSS background colours/images." default:"true"`
	MarginInches    float64 `arg:"margin_inches"     desc:"Uniform page margin in inches (default 0.4)."`
}

func (a *DocumentRenderHTML) ActionName() string { return "render_html" }
func (a *DocumentRenderHTML) Doc() string {
	return "Render an inline HTML string to a PDF document (no project page required)."
}
func (a *DocumentRenderHTML) Example() any {
	return &DocumentRenderHTML{
		HTML:     "<html><body style=\"font-family:sans-serif\"><h1>Receipt</h1><p>Total: $42.00</p></body></html>",
		Filename: "receipt",
	}
}
func (a *DocumentRenderHTML) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return documentRenderHTML(ctx, a.HTML, a.Filename, a.PageSize, a.Landscape, a.PrintBackground, a.MarginInches)
}

// DocumentList lists generated PDF documents.
type DocumentList struct{}

func (a *DocumentList) Doc() string    { return "List generated PDF documents." }
func (a *DocumentList) ReadOnly() bool { return true }
func (a *DocumentList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return documentList(ctx)
}
