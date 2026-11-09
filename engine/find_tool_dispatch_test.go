package engine

// find_tool is answered locally, ahead of the registry dispatch path - which is how it came
// to miss the two things every other tool call gets: loose-JSON repair on its arguments,
// and a cap on the payload it appends to the transcript. Both matter more here than
// elsewhere. A rejected find_tool costs a turn AND leaves the model without the schema it
// was reaching for, and a no-argument browse is the one call whose size scales with the
// registry rather than with what was asked for.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

func findToolCall(id, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: tools.FindToolName, Arguments: args}
}

// runFindTool drives one find_tool call through the real dispatch path and returns the
// payload that landed in the transcript.
func runFindTool(t *testing.T, args string) string {
	t.Helper()
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(findToolCall("f1", args)),
		enginetest.Text("done"),
	}}
	s := testSession(t, p)
	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool && m.ToolCallID == "f1" {
			return m.Content
		}
	}
	t.Fatal("no find_tool result landed in the transcript")
	return ""
}

// TestFindToolRepairsLooseArguments: a fenced argument blob is the shape a weaker model
// emits, and every registry tool already survives it. find_tool must repair it rather than
// reject it as "invalid arguments JSON", which reads to the model as "that tool does not exist".
func TestFindToolRepairsLooseArguments(t *testing.T) {
	payload := runFindTool(t, "```json\n{\"names\": [\"test_probe_tool\",]}\n```")

	var out tools.FindToolResult
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("find_tool payload is not JSON: %v (%q)", err, payload)
	}
	if out.Error != "" {
		t.Fatalf("find_tool rejected repairable arguments: %q", out.Error)
	}
	// test_probe_tool is already preloaded by buildSpecForTest, so the call resolves to
	// already_loaded - which still proves the arguments parsed.
	if len(out.Loaded) == 0 && len(out.AlreadyLoaded) == 0 {
		t.Errorf("neither loaded nor already_loaded is populated: %s", payload)
	}
	if len(out.NotFound) > 0 {
		t.Errorf("not_found=%v - the repaired name did not resolve", out.NotFound)
	}
}

// TestFindToolAcceptsBareStringName: "names" given as a bare string rather than a list.
// Cheap to accept, and the alternative is a wasted turn over a bracket.
func TestFindToolAcceptsBareStringName(t *testing.T) {
	payload := runFindTool(t, `{"names": "test_probe_tool"}`)

	var out tools.FindToolResult
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("find_tool payload is not JSON: %v", err)
	}
	if len(out.Loaded) == 0 && len(out.AlreadyLoaded) == 0 {
		t.Errorf("a bare-string name did not resolve: %s", payload)
	}
}

// TestFindToolBrowseOmitsWhenToUse: a no-argument browse answers "what exists". Carrying
// every tool's full routing prose answers a question nobody asked, at ~31KB across the
// registry, permanently in the transcript.
func TestFindToolBrowseOmitsWhenToUse(t *testing.T) {
	payload := runFindTool(t, `{}`)

	var out tools.FindToolResult
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("find_tool payload is not JSON: %v", err)
	}
	if len(out.Matches) == 0 {
		t.Fatal("browse returned no matches")
	}
	for _, m := range out.Matches {
		if m.WhenToUse != "" {
			t.Errorf("browse entry %q carries when_to_use (%d bytes); browse is names + one-liners "+
				"+ action verbs, and the prose arrives with the schema on load", m.Name, len(m.WhenToUse))
			break
		}
		if m.OneLiner == "" || len(m.Actions) == 0 {
			t.Errorf("browse entry %q lost the one-liner or action verbs - the model cannot choose from it", m.Name)
			break
		}
	}
	if !strings.Contains(out.Note, "find_tool(names:") {
		t.Errorf("browse note does not tell the model how to load what it picked: %q", out.Note)
	}
}

// TestFindToolNamesBypassRepeatGuard: a call carrying names is a load, not a search.
// The repeat-query guard must neither short-circuit it (the schemas would never load)
// nor record its query as a fruitless search (the names path never runs the query).
func TestFindToolNamesBypassRepeatGuard(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(findToolCall("f1", `{"names": ["test_probe_tool"], "query": "frobnicate widget gadget"}`)),
		enginetest.Calls(findToolCall("f2", `{"query": "frobnicate widget gadget"}`)),
		enginetest.Text("done"),
	}}
	s := testSession(t, p)
	res, err := s.Run(context.Background(), buildSpecForTest())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	payloads := map[string]string{}
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool {
			payloads[m.ToolCallID] = m.Content
		}
	}

	var out tools.FindToolResult
	if err := json.Unmarshal([]byte(payloads["f1"]), &out); err != nil {
		t.Fatalf("names+query payload is not JSON: %v", err)
	}
	if len(out.Loaded) == 0 && len(out.AlreadyLoaded) == 0 {
		t.Errorf("names+query call resolved no tool - the repeat guard swallowed the load: %s", payloads["f1"])
	}
	if strings.Contains(out.Note, "already searched") {
		t.Errorf("names+query call was answered as a repeated search: %q", out.Note)
	}

	// The names call must not have recorded its unsearched query as a miss: the
	// first pure query still runs the real search, not the repeat verdict.
	var second tools.FindToolResult
	if err := json.Unmarshal([]byte(payloads["f2"]), &second); err != nil {
		t.Fatalf("follow-up query payload is not JSON: %v", err)
	}
	if strings.Contains(second.Note, "already searched") {
		t.Errorf("query after a names+query call was refused as repeated: %q", second.Note)
	}
}

// TestFindToolPayloadIsCapped: every registry result is truncated into the transcript;
// find_tool was appended raw. It is not a registered ToolDef, so it needs its own cap
// rather than inheriting one.
func TestFindToolPayloadIsCapped(t *testing.T) {
	payload := runFindTool(t, `{}`)
	if len(payload) > findToolMaxResult+300 {
		t.Errorf("browse payload is %d bytes, past the %d-byte cap plus its truncation footer",
			len(payload), findToolMaxResult)
	}
}

// TestFindToolCountsAsReadOnly: find_tool is a pure lookup. It is not in the
// registry, so without its own case it falls through the action fallback and
// counts as a write - resetting the read-only streak and reporting Writes > 0
// to the scheduled-run no-op backoff.
func TestFindToolCountsAsReadOnly(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("done")}}
	s := testSession(t, p)
	if !s.isReadOnlyToolCall(llm.ToolCall{Name: tools.FindToolName}, map[string]any{}) {
		t.Fatal("find_tool must classify as read-only")
	}
}
