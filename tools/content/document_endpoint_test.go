package content

import (
	"strings"
	"testing"

	"openfabrica/tools/tooltest"
)

func TestCreateDocumentEndpoint(t *testing.T) {
	h := tooltest.New(t)
	res := h.MustOK("manage_documents", map[string]any{
		"action": "create_endpoint", "endpoint_path": "invoice",
		"template_html": `<h1>Invoice {{number}}</h1>{{#each items}}<p>{{name}}</p>{{/each}}`,
		"filename":      "invoice-{{number}}",
	})
	data, _ := res.Data.(map[string]any)
	if data["url"] != "/api/invoice/pdf" {
		t.Errorf("url = %v", data["url"])
	}
	var auth bool
	var kind string
	if err := h.DB().QueryRow(`SELECT requires_auth FROM of_document_endpoints WHERE path = 'invoice'`).Scan(&auth); err != nil || !auth {
		t.Errorf("a document endpoint must default to requiring auth: %v %v", auth, err)
	}
	if err := h.DB().QueryRow(`SELECT kind FROM of_endpoints WHERE path = 'invoice' AND sub_segment = 'pdf'`).Scan(&kind); err != nil || kind != "document" {
		t.Errorf("the spine must record the route: %q %v", kind, err)
	}

	list := h.MustOK("manage_documents", map[string]any{"action": "list_endpoints"})
	if got := tooltest.DataJSON(list); !strings.Contains(got, "/api/invoice/pdf") {
		t.Errorf("list = %s", got)
	}
}

func TestCreateDocumentEndpointRejectsBadTemplates(t *testing.T) {
	h := tooltest.New(t)
	for name, args := range map[string]map[string]any{
		"unclosed tag":   {"action": "create_endpoint", "endpoint_path": "a", "template_html": "<p>{{name"},
		"unclosed block": {"action": "create_endpoint", "endpoint_path": "b", "template_html": "{{#each x}}<p>"},
		"empty":          {"action": "create_endpoint", "endpoint_path": "c", "template_html": "   "},
		"bad filename":   {"action": "create_endpoint", "endpoint_path": "d", "template_html": "<p>ok</p>", "filename": "x{{"},
	} {
		if res := h.Call("manage_documents", args); res.Success {
			t.Errorf("%s: want a failure", name)
		}
	}
}
