package tools

import "testing"

// normalizeArgAliases copies a known alias ("path") into the canonical key
// ("file_path") so a weak model's natural call resolves instead of failing the
// required-arg check and burning recovery turns - but only when the tool declares
// the canonical param, and never overwriting a value already supplied.
func TestNormalizeArgAliases_PathToFilePath(t *testing.T) {
	fileTool := &ToolDef{Params: []Param{{Name: "file_path", Type: "string", Required: true}}}

	t.Run("alias fills an absent canonical", func(t *testing.T) {
		args := map[string]any{"action": "get", "path": "js/app.js"}
		normalizeArgAliases(fileTool, args)
		if args["file_path"] != "js/app.js" {
			t.Fatalf("path alias not copied to file_path: %+v", args)
		}
		// The required check now passes off the aliased value.
		if r := validateArgs(fileTool, args); r != nil {
			t.Fatalf("validateArgs should pass after aliasing, got %q", r.Error)
		}
	})

	t.Run("alias never clobbers a supplied canonical", func(t *testing.T) {
		args := map[string]any{"file_path": "good.js", "path": "wrong.js"}
		normalizeArgAliases(fileTool, args)
		if args["file_path"] != "good.js" {
			t.Fatalf("alias must not overwrite a supplied file_path: %+v", args)
		}
	})

	t.Run("alias fills an empty canonical", func(t *testing.T) {
		args := map[string]any{"file_path": "", "path": "x.js"}
		normalizeArgAliases(fileTool, args)
		if args["file_path"] != "x.js" {
			t.Fatalf("empty file_path should be filled from the path alias: %+v", args)
		}
	})

	t.Run("untouched for a tool that declares no file_path", func(t *testing.T) {
		other := &ToolDef{Params: []Param{{Name: "path", Type: "string"}}}
		args := map[string]any{"path": "/providers/x"}
		normalizeArgAliases(other, args)
		if _, leaked := args["file_path"]; leaked {
			t.Fatalf("must not inject file_path for a tool that doesn't declare it: %+v", args)
		}
	})
}

// TestNormalizeArgAliases_SearchStringSynonyms covers the search tools, each of
// which names the search string differently (query_text / fts_query /
// semantic_query), so a model reaching for the natural "query" (or a sibling's
// name) resolves onto whichever param the called tool actually declares.
func TestNormalizeArgAliases_SearchStringSynonyms(t *testing.T) {
	dataTool := &ToolDef{Params: []Param{{Name: "query_text", Type: "string", Required: true}}}    // manage_data search
	ftsTool := &ToolDef{Params: []Param{{Name: "fts_query", Type: "string", Required: true}}}      // manage_keyword_search
	vecTool := &ToolDef{Params: []Param{{Name: "semantic_query", Type: "string", Required: true}}} // manage_vector_search
	webTool := &ToolDef{Params: []Param{{Name: "query", Type: "string", Required: true}}}          // manage_web_search (native)

	t.Run("query fills query_text and passes the required check", func(t *testing.T) {
		args := map[string]any{"action": "search", "query": "bread"}
		normalizeArgAliases(dataTool, args)
		if args["query_text"] != "bread" {
			t.Fatalf("query not aliased to query_text: %+v", args)
		}
		if r := validateArgs(dataTool, args); r != nil {
			t.Fatalf("validateArgs should pass after aliasing, got %q", r.Error)
		}
	})

	t.Run("query fills fts_query", func(t *testing.T) {
		args := map[string]any{"query": "bread"}
		normalizeArgAliases(ftsTool, args)
		if args["fts_query"] != "bread" {
			t.Fatalf("query not aliased to fts_query: %+v", args)
		}
	})

	t.Run("a sibling's name fills the declared canonical", func(t *testing.T) {
		// A model sends the keyword tool's name to the vector tool.
		args := map[string]any{"fts_query": "bread"}
		normalizeArgAliases(vecTool, args)
		if args["semantic_query"] != "bread" {
			t.Fatalf("fts_query not aliased to semantic_query: %+v", args)
		}
	})

	t.Run("alias never clobbers a supplied canonical", func(t *testing.T) {
		args := map[string]any{"fts_query": "good", "query": "wrong"}
		normalizeArgAliases(ftsTool, args)
		if args["fts_query"] != "good" {
			t.Fatalf("alias must not overwrite a supplied fts_query: %+v", args)
		}
	})

	t.Run("web tool keeps its native query untouched", func(t *testing.T) {
		args := map[string]any{"query": "bread"}
		normalizeArgAliases(webTool, args)
		if _, leaked := args["query_text"]; leaked {
			t.Fatalf("must not inject query_text for the web tool: %+v", args)
		}
		if args["query"] != "bread" {
			t.Fatalf("native query must be preserved: %+v", args)
		}
	})
}
