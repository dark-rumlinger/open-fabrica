package project

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"time"

	"openfabrica/internal/netguard"
	"openfabrica/internal/stringutil"
	"openfabrica/tools"
	"openfabrica/tools/internal/provider"
)

const (
	maxDescribedOps  = 150             // cap operations returned so the result stays bounded
	describeParamCap = 12              // cap params listed per operation
	maxDescribeBody  = 2 * 1024 * 1024 // cap the fetched doc at 2 MiB
)

// ProviderDescribe fetches a registered provider's OpenAPI/Swagger (JSON)
// document and returns a compact list of callable operations as result data, so
// the model can form correct request paths/bodies instead of guessing. Mirrors
// find_tool's discipline: the description is returned as DATA, never injected
// into the prompt.
type ProviderDescribe struct {
	Provider string `arg:"provider,required" desc:"Registered provider name to describe."`
	DocsURL  string `arg:"docs_url"          desc:"OpenAPI/Swagger JSON URL. Defaults to the provider's stored api_docs when that is a URL."`
	Filter   string `arg:"filter"            desc:"Optional case-insensitive substring; keep only operations whose path or summary matches."`
}

func (a *ProviderDescribe) ReadOnly() bool { return true }
func (a *ProviderDescribe) Doc() string {
	return "Fetch a provider's OpenAPI/Swagger doc and return a compact list of callable operations."
}
func (a *ProviderDescribe) Example() any {
	return &ProviderDescribe{Provider: "acme", DocsURL: "https://api.acme.com/openapi.json"}
}

func (a *ProviderDescribe) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	creds, errR := provider.Load(ctx, a.Provider)
	if errR != nil {
		return errR, nil
	}

	docURL := strings.TrimSpace(a.DocsURL)
	if docURL == "" {
		var apiDocs string
		_ = ctx.DB.QueryRow("SELECT api_docs FROM of_service_providers WHERE name = ?", a.Provider).Scan(&apiDocs)
		apiDocs = strings.TrimSpace(apiDocs)
		if strings.HasPrefix(apiDocs, "http://") || strings.HasPrefix(apiDocs, "https://") {
			docURL = apiDocs
		}
	}
	if docURL == "" {
		return tools.Fail("no OpenAPI doc URL: pass docs_url, or set the provider's api_docs to a JSON OpenAPI/Swagger URL"), nil
	}

	// Same default-deny SSRF policy as request: a private doc host is reachable
	// only when the provider opted in, and only for its own host.
	allowHost := ""
	if providerAllowsPrivateHost(ctx, a.Provider) {
		if pu, err := neturl.Parse(creds.BaseURL); err == nil {
			allowHost = pu.Hostname()
		}
	}
	if err := netguard.ValidateExternalURLAllowingHost(docURL, allowHost); err != nil {
		return tools.Fail("blocked docs URL: %v", err), nil
	}

	req, err := http.NewRequestWithContext(ctx.Context(), http.MethodGet, docURL, nil)
	if err != nil {
		return tools.Fail("creating request: %v", err), nil
	}
	req.Header.Set("Accept", "application/json")
	client := netguard.SSRFSafeClientAllowingHost(allowHost)
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return tools.Fail("fetching docs: %v", err), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tools.Fail("docs URL returned %s", resp.Status), nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDescribeBody))
	if err != nil {
		return nil, fmt.Errorf("reading docs: %w", err)
	}

	var doc openapiDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return tools.Fail("not a JSON OpenAPI/Swagger document (YAML is not supported - provide a JSON doc URL): %v", err), nil
	}
	if len(doc.Paths) == 0 {
		return tools.Fail("document has no paths; is this an OpenAPI/Swagger doc?"), nil
	}

	ops := extractOps(&doc, strings.ToLower(strings.TrimSpace(a.Filter)))
	total := len(ops)
	truncated := false
	if len(ops) > maxDescribedOps {
		ops = ops[:maxDescribedOps]
		truncated = true
	}

	data := map[string]any{
		"provider":        a.Provider,
		"title":           doc.Info.Title,
		"version":         doc.Info.Version,
		"base_url":        effectiveBaseURL(&doc, creds.BaseURL),
		"operation_count": total,
		"operations":      ops,
	}
	if truncated {
		data["truncated"] = true
		data["hint"] = fmt.Sprintf("showing first %d of %d operations; pass filter to narrow", maxDescribedOps, total)
	}
	return tools.Ok(data), nil
}

// openapiDoc captures the few fields describe needs from an OpenAPI v3 or
// Swagger v2 document. Path entries are left raw because a path item also holds
// non-operation keys (a shared `parameters` array, `$ref`, etc.); only known
// HTTP-method keys are decoded as operations.
type openapiDoc struct {
	Info struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	BasePath string                                `json:"basePath"`
	Paths    map[string]map[string]json.RawMessage `json:"paths"`
}

type openapiOp struct {
	Summary     string `json:"summary"`
	OperationID string `json:"operationId"`
	Parameters  []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"parameters"`
}

type describedOp struct {
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Summary string   `json:"summary,omitempty"`
	OpID    string   `json:"operation_id,omitempty"`
	Params  []string `json:"params,omitempty"`
}

var describeMethods = []string{"get", "post", "put", "delete", "patch"}

func extractOps(doc *openapiDoc, filter string) []describedOp {
	var ops []describedOp
	for path, methods := range doc.Paths {
		for _, m := range describeMethods {
			raw, ok := methods[m]
			if !ok {
				continue
			}
			var op openapiOp
			if err := json.Unmarshal(raw, &op); err != nil {
				continue
			}
			summary := op.Summary
			if summary == "" {
				summary = op.OperationID
			}
			entry := describedOp{
				Method:  strings.ToUpper(m),
				Path:    path,
				Summary: stringutil.Truncate(summary, 140),
				OpID:    op.OperationID,
			}
			for i, pr := range op.Parameters {
				if i >= describeParamCap {
					entry.Params = append(entry.Params, "...")
					break
				}
				tag := pr.Name + "(" + pr.In + ")"
				if pr.Required {
					tag += "*"
				}
				entry.Params = append(entry.Params, tag)
			}
			if filter != "" &&
				!strings.Contains(strings.ToLower(entry.Path), filter) &&
				!strings.Contains(strings.ToLower(entry.Summary), filter) {
				continue
			}
			ops = append(ops, entry)
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return ops[i].Method < ops[j].Method
	})
	return ops
}

// effectiveBaseURL prefers the doc's declared server (OpenAPI v3) or basePath
// (Swagger v2), falling back to the provider's registered base_url.
func effectiveBaseURL(doc *openapiDoc, fallback string) string {
	if len(doc.Servers) > 0 && strings.TrimSpace(doc.Servers[0].URL) != "" {
		return doc.Servers[0].URL
	}
	if doc.BasePath != "" {
		return strings.TrimRight(fallback, "/") + doc.BasePath
	}
	return fallback
}
