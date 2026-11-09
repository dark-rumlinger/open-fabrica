package project

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/tools"
)

// sampleOpenAPI exercises the v3 shape plus a path-level `parameters` array (a
// non-operation key under a path item) to confirm only HTTP-method keys are
// decoded as operations.
const sampleOpenAPI = `{
  "openapi": "3.0.0",
  "info": { "title": "Acme API", "version": "1.2.3" },
  "servers": [{ "url": "https://api.acme.com/v1" }],
  "paths": {
    "/things": {
      "parameters": [{ "name": "shared", "in": "query" }],
      "get":  { "summary": "List things",   "operationId": "listThings", "parameters": [{ "name": "limit", "in": "query", "required": false }] },
      "post": { "summary": "Create thing",  "operationId": "createThing" }
    },
    "/things/{id}": {
      "get": { "summary": "Get thing", "parameters": [{ "name": "id", "in": "path", "required": true }] }
    }
  }
}`

func openapiServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleOpenAPI))
	}))
}

func TestProviderDescribe(t *testing.T) {
	srv := openapiServer(t)
	defer srv.Close()

	db := providerTestDB(t)
	addProvider(t, db, "acme", srv.URL, 1) // allow_private_host so loopback docs are reachable
	ctx := &tools.ToolContext{DB: db, Ctx: context.Background()}

	res, err := (&ProviderDescribe{Provider: "acme", DocsURL: srv.URL + "/openapi.json"}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("describe failed: %+v", res)
	}
	data := res.Data.(map[string]any)
	if data["title"] != "Acme API" || data["version"] != "1.2.3" {
		t.Fatalf("info mismatch: %+v", data)
	}
	if data["base_url"] != "https://api.acme.com/v1" {
		t.Fatalf("base_url from servers expected, got %v", data["base_url"])
	}

	ops := data["operations"].([]describedOp)
	if len(ops) != 3 {
		t.Fatalf("want 3 operations, got %d: %+v", len(ops), ops)
	}
	// Sorted by path then method: GET /things, POST /things, GET /things/{id}.
	if ops[0].Method != "GET" || ops[0].Path != "/things" {
		t.Fatalf("first op = %+v", ops[0])
	}
	var byID *describedOp
	for i := range ops {
		if ops[i].Path == "/things/{id}" {
			byID = &ops[i]
		}
	}
	if byID == nil || len(byID.Params) == 0 || byID.Params[0] != "id(path)*" {
		t.Fatalf("expected required path param id(path)*, got %+v", byID)
	}

	// Filter keeps only matching operations (by path or summary).
	res2, _ := (&ProviderDescribe{Provider: "acme", DocsURL: srv.URL + "/o", Filter: "create"}).Run(ctx)
	ops2 := res2.Data.(map[string]any)["operations"].([]describedOp)
	if len(ops2) != 1 || ops2[0].Method != "POST" {
		t.Fatalf("filter 'create' should match only POST /things, got %+v", ops2)
	}
}

func TestProviderDescribeSSRFBlocked(t *testing.T) {
	srv := openapiServer(t)
	defer srv.Close()

	db := providerTestDB(t)
	addProvider(t, db, "acme", srv.URL, 0) // no opt-in: loopback docs URL must be blocked
	ctx := &tools.ToolContext{DB: db, Ctx: context.Background()}

	res, _ := (&ProviderDescribe{Provider: "acme", DocsURL: srv.URL + "/openapi.json"}).Run(ctx)
	if res.Success || !strings.Contains(res.Error, "blocked") {
		t.Fatalf("expected loopback docs URL blocked without opt-in, got %+v", res)
	}
}
