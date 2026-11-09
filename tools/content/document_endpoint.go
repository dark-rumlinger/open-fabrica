package content

import (
	"fmt"
	"strings"

	"openfabrica/internal/doctemplate"
	"openfabrica/internal/endpoints"
	"openfabrica/tools"
	"openfabrica/tools/api"
)

// Runtime PDF endpoints for manage_documents. render_url / render_html make a
// document once, while building; create_endpoint is for documents made per
// request (an invoice for order 17): a stored template plus POST /api/<path>/pdf.

const maxDocumentTemplateBytes = 512 << 10

// DocumentCreateEndpoint mounts POST /api/<path>/pdf over an HTML template.
type DocumentCreateEndpoint struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Endpoint path suffix, no /api/ prefix (e.g. \"invoice\" serves POST /api/invoice/pdf)."`
	TemplateHTML string `arg:"template_html,required" desc:"Full HTML document with inline styles. {{name}} inserts an escaped value (dotted paths: {{customer.name}}); {{#each items}}<tr><td>{{name}}</td><td>{{@index}}</td></tr>{{/each}} repeats for an array ({{this}} for a plain element, {{../total}} for the outer scope). No scripts run."`
	Filename     string `arg:"filename"      desc:"Download filename; may use {{values}} (e.g. \"invoice-{{number}}\"). \".pdf\" is appended." default:"document"`
	PageSize     string `arg:"page_size"     desc:"Paper size." enum:"A4,Letter,Legal" default:"A4"`
	Landscape    bool   `arg:"landscape"     desc:"Landscape orientation."`
	RequiresAuth bool   `arg:"requires_auth" desc:"Require a signed-in user or project key. Leave on unless the document is meant for anonymous visitors." default:"true"`
	RateLimit    int    `arg:"rate_limit"    desc:"Renders per minute per visitor (each starts a headless browser)." default:"10" min:"1" max:"600"`
}

func (a *DocumentCreateEndpoint) ActionName() string { return "create_endpoint" }
func (a *DocumentCreateEndpoint) Doc() string {
	return "Serve POST /api/<path>/pdf: fill an HTML template with the request's data and return the PDF, for documents made per request (invoices, receipts, tickets)."
}
func (a *DocumentCreateEndpoint) Example() any {
	return &DocumentCreateEndpoint{
		EndpointPath: "invoice",
		TemplateHTML: `<html><body style="font-family:sans-serif"><h1>Invoice {{number}}</h1><p>{{customer.name}}</p><table>{{#each items}}<tr><td>{{name}}</td><td>{{price}}</td></tr>{{/each}}</table></body></html>`,
		Filename:     "invoice-{{number}}",
		RequiresAuth: true,
	}
}
func (a *DocumentCreateEndpoint) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	path, errRes := tools.ValidateAndNormalizeEndpointPath(a.EndpointPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	if strings.TrimSpace(a.TemplateHTML) == "" {
		return tools.Fail("template_html is required"), nil
	}
	if len(a.TemplateHTML) > maxDocumentTemplateBytes {
		return tools.Fail("template_html exceeds %d bytes (got %d)", maxDocumentTemplateBytes, len(a.TemplateHTML)), nil
	}
	if _, err := doctemplate.Render(a.TemplateHTML, map[string]any{}); err != nil {
		return tools.Fail("template_html is not valid: %v", err), nil
	}
	filename := a.Filename
	if strings.TrimSpace(filename) == "" {
		filename = "document"
	}
	if _, err := doctemplate.Render(filename, map[string]any{}); err != nil {
		return tools.Fail("filename is not valid: %v", err), nil
	}
	pageSize := a.PageSize
	if pageSize == "" {
		pageSize = "A4"
	}
	limit := a.RateLimit
	if limit <= 0 {
		limit = 10
	}

	if err := api.ClaimEndpoint(ctx, endpoints.KindDocument, path, nil); err != nil {
		return tools.FailErr(err), nil
	}
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_document_endpoints (path, template_html, filename, page_size, landscape, requires_auth, rate_limit)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET template_html = excluded.template_html, filename = excluded.filename,
		   page_size = excluded.page_size, landscape = excluded.landscape,
		   requires_auth = excluded.requires_auth, rate_limit = excluded.rate_limit`,
		path, a.TemplateHTML, filename, pageSize, a.Landscape, a.RequiresAuth, limit,
	); err != nil {
		return nil, fmt.Errorf("create_endpoint: %w", err)
	}
	return tools.Ok(map[string]any{
		"endpoint_path": path,
		"url":           "/api/" + path + "/pdf",
		"requires_auth": a.RequiresAuth,
		"how_to_call":   "POST /api/" + path + "/pdf with JSON {\"data\": {...}}; the response body is the PDF (application/pdf). Fetch it with the auth token, then open the blob: `const blob = await (await apiFetch('/api/" + path + "/pdf', {method:'POST', body:{data}})).blob(); window.open(URL.createObjectURL(blob))`.",
		"note":          "Needs Chrome or Edge on the server. Values are escaped and scripts do not run, so put every style inline in the template.",
	}), nil
}

// DocumentListEndpoints lists the runtime PDF endpoints.
type DocumentListEndpoints struct{}

func (a *DocumentListEndpoints) ActionName() string { return "list_endpoints" }
func (a *DocumentListEndpoints) Doc() string        { return "List the runtime PDF endpoints." }
func (a *DocumentListEndpoints) ReadOnly() bool     { return true }
func (a *DocumentListEndpoints) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(`SELECT path, filename, page_size, landscape, requires_auth, rate_limit FROM of_document_endpoints ORDER BY path`)
	if err != nil {
		return tools.Fail("listing document endpoints: %s", err.Error()), nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var path, filename, size string
		var landscape, auth bool
		var limit int
		if rows.Scan(&path, &filename, &size, &landscape, &auth, &limit) == nil {
			out = append(out, map[string]any{
				"endpoint_path": path, "url": "/api/" + path + "/pdf", "filename": filename,
				"page_size": size, "landscape": landscape, "requires_auth": auth, "rate_limit": limit,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("listing document endpoints: %s", err.Error()), nil
	}
	return tools.Ok(map[string]any{"endpoints": out}), nil
}
