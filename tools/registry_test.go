package tools

import (
	"strings"
	"testing"

	"openfabrica/providers/llm"
)

// newTestTool builds a minimal ToolDef and inserts it into r. Enough fields are
// populated to exercise the catalogue / search / embedding paths: one action
// (for actionNames) with a Summary (for the action search haystack).
func newTestTool(r *Registry, name, category, oneLiner, whenToUse, capability, action, actionSummary string) {
	def := &ToolDef{
		Name:               name,
		Category:           category,
		OneLiner:           oneLiner,
		WhenToUse:          whenToUse,
		RequiresCapability: capability,
		Actions:            map[string]ActionHandler{action: func(*ToolContext, map[string]any) (*Result, error) { return Ok(nil), nil }},
		ActionMeta:         map[string]ActionMeta{action: {Summary: actionSummary}},
	}
	r.mu.Lock()
	r.tools[name] = def
	r.mu.Unlock()
}

// TestIndexOmitsWhenLine: the catalogue carries one line per tool (name,
// one-liner, action verbs) but NOT the verbose when-to-use - that is the bulk of
// the catalogue and would be re-billed every turn on a provider without prompt
// caching, so it loads on demand via find_tool (LookupDocs) instead.
func TestIndexOmitsWhenLine(t *testing.T) {
	r := NewRegistry()
	newTestTool(r, "manage_email", "messaging", "Send transactional email.",
		"Use for one-off email to a known address.\nNot for in-app alerts.", "", "send", "deliver an email")

	idx := r.Index(llm.Capabilities{}, "")
	if !strings.Contains(idx, "- `manage_email`: Send transactional email. | actions: send") {
		t.Fatalf("index missing the tool line:\n%s", idx)
	}
	if strings.Contains(idx, "when:") || strings.Contains(idx, "one-off email") {
		t.Fatalf("index must NOT inline when-to-use (it loads via find_tool):\n%s", idx)
	}
}

// TestHybridSearch_NilSemanticIsKeywordOnly pins the no-embeddings-provider path:
// with no semantic ranking to fuse, the result is whatever keyword scoring found,
// in its order.
func TestHybridSearch_NilSemanticIsKeywordOnly(t *testing.T) {
	r := NewRegistry()
	newTestTool(r, "manage_email", "messaging", "Send transactional email.", "Email.", "", "send", "deliver email")
	newTestTool(r, "manage_sms", "messaging", "Send a text message.", "SMS.", "", "send", "deliver sms text")

	got := r.HybridSearch("send", nil, llm.Capabilities{}, 10, "")
	if len(got) != 2 {
		t.Fatalf("keyword-only search for %q matched %d tools, want both: %+v", "send", len(got), got)
	}
}

// TestHybridSearch_SemanticOnlyHitSurfaces: a tool that the strict-AND keyword
// pass drops entirely must still surface when semantic ranking places it high -
// neither pass alone decides the result.
func TestHybridSearch_SemanticOnlyHitSurfaces(t *testing.T) {
	r := NewRegistry()
	// Query "newsletter" matches none of these by substring, so keyword search
	// returns nothing. Semantic ranking (supplied here) puts manage_feeds first.
	newTestTool(r, "manage_feeds", "api", "Publish RSS and Atom syndication.", "Syndicate posts.", "", "create", "build a feed")
	newTestTool(r, "manage_email", "messaging", "Send transactional email.", "Email.", "", "send", "deliver email")

	caps := llm.Capabilities{}
	if kw := r.HybridSearch("newsletter", nil, caps, 10, ""); len(kw) != 0 {
		t.Fatalf("expected keyword search to miss 'newsletter', got %+v", kw)
	}
	semanticOrder := []string{"manage_feeds", "manage_email"}
	hy := r.HybridSearch("newsletter", semanticOrder, caps, 10, "")
	if len(hy) == 0 || hy[0].Name != "manage_feeds" {
		t.Fatalf("expected semantic-only hit manage_feeds first, got %+v", hy)
	}
}

// TestHybridSearch_FusionRewardsBothLists: a tool ranked highly by BOTH keyword
// and semantic should outrank one that only a single list favors.
func TestHybridSearch_FusionRewardsBothLists(t *testing.T) {
	r := NewRegistry()
	newTestTool(r, "manage_sms", "messaging", "Send a text message to a phone.", "Texting.", "", "send", "send sms text")
	newTestTool(r, "manage_email", "messaging", "Send email.", "Email.", "", "send", "send email")

	caps := llm.Capabilities{}
	// Keyword "text" hits manage_sms (in one-liner + action) but not manage_email.
	// Semantic order also puts manage_sms first. Fusion must keep manage_sms on top.
	hy := r.HybridSearch("send text", []string{"manage_sms", "manage_email"}, caps, 10, "")
	if len(hy) == 0 || hy[0].Name != "manage_sms" {
		t.Fatalf("expected manage_sms ranked first by fusion, got %+v", hy)
	}
}

// TestEmbeddingProfiles_GatesAndContent: profiles exclude capability-gated tools
// and carry the tool's name + prose + action vocabulary for embedding.
func TestEmbeddingProfiles_GatesAndContent(t *testing.T) {
	r := NewRegistry()
	newTestTool(r, "manage_email", "messaging", "Send transactional email.", "Use for email.", "", "send", "deliver email")
	newTestTool(r, "manage_vector_search", "data", "Semantic search over a table.", "Needs embeddings.", "embeddings", "semantic_search", "vector similarity")

	// No embeddings capability: the gated tool must be absent.
	profiles := r.EmbeddingProfiles(llm.Capabilities{}, "")
	if len(profiles) != 1 || profiles[0].Name != "manage_email" {
		t.Fatalf("expected only manage_email, got %+v", profiles)
	}
	if !strings.Contains(profiles[0].Text, "manage_email") ||
		!strings.Contains(profiles[0].Text, "transactional email") ||
		!strings.Contains(profiles[0].Text, "deliver email") {
		t.Fatalf("profile text missing name/prose/action vocab: %q", profiles[0].Text)
	}

	// With embeddings present, the gated tool is included.
	profiles = r.EmbeddingProfiles(llm.Capabilities{HasEmbeddings: true}, "")
	if len(profiles) != 2 {
		t.Fatalf("expected both tools with embeddings capability, got %+v", profiles)
	}
}

// The embedding text of a tool with several actions must be byte-identical on every
// call: it is hashed into the catalogue embedding cache key, so a map-order haystack
// would miss the cache and re-embed the whole catalogue on every find_tool query.
func TestEmbeddingProfiles_MultiActionTextIsDeterministic(t *testing.T) {
	r := NewRegistry()
	handler := func(*ToolContext, map[string]any) (*Result, error) { return Ok(nil), nil }
	actions := map[string]ActionHandler{}
	meta := map[string]ActionMeta{}
	for _, a := range []string{"create", "update", "delete", "list", "get", "export", "import", "check"} {
		actions[a] = handler
		meta[a] = ActionMeta{Summary: "does " + a}
	}
	r.mu.Lock()
	r.tools["manage_many"] = &ToolDef{Name: "manage_many", Category: "data", OneLiner: "Many actions.", Actions: actions, ActionMeta: meta}
	r.mu.Unlock()

	first := r.EmbeddingProfiles(llm.Capabilities{}, "")[0].Text
	for i := 0; i < 50; i++ {
		if got := r.EmbeddingProfiles(llm.Capabilities{}, "")[0].Text; got != first {
			t.Fatalf("embedding text changed between calls:\n%q\n%q", first, got)
		}
	}
}

// A query is ranked on its meaningful words: plurals fold, filler drops, and a tool
// needs most of the words, not all. "send emails to customers" must find the email
// tool even though no tool mentions customers.
func TestKeywordRank_ToleratesOneStrayWord(t *testing.T) {
	r := NewRegistry()
	newTestTool(r, "manage_email", "messaging", "Send transactional email.", "Use for email.", "", "send", "deliver email")
	newTestTool(r, "manage_schema", "data", "Create tables.", "Use for tables.", "", "create_table", "make a table")

	got := r.HybridSearch("send emails to customers", nil, llm.Capabilities{}, 0, "")
	if len(got) == 0 || got[0].Name != "manage_email" {
		t.Fatalf("expected manage_email first, got %+v", got)
	}
	for _, e := range got {
		if e.Name == "manage_schema" {
			t.Fatalf("an unrelated tool matched: %+v", got)
		}
	}
	if got := r.HybridSearch("quantum teleportation hologram", nil, llm.Capabilities{}, 0, ""); len(got) != 0 {
		t.Fatalf("a query with no matching word must stay empty, got %+v", got)
	}
}

func TestFindQueryLog_SingleWordCorrectionIsNotARepeat(t *testing.T) {
	var l FindQueryLog
	l.Record("send emails to customers")
	if prior := l.Repeat("email"); prior != "" {
		t.Fatalf("a one-word correction of a long miss was refused as a repeat of %q", prior)
	}
	if prior := l.Repeat("send emails customers again"); prior == "" {
		t.Fatal("a reword of the same words must still be refused")
	}
}

func TestServiceFindTool_NamesAreNormalizedAndSuggested(t *testing.T) {
	r := NewRegistry()
	newTestTool(r, "manage_files", "content", "Files.", "Use for files.", "", "save", "save a file")
	res, schemas := ServiceFindTool(t.Context(), r, llm.Capabilities{}, "", FindToolArgs{Names: []string{"Manage_Files", "manage_files", "manage_file"}}, nil, nil)
	if len(schemas) != 1 || len(res.Loaded) != 1 {
		t.Fatalf("a repeated or differently-cased name must load once: loaded=%v schemas=%d", res.Loaded, len(schemas))
	}
	if len(res.NotFound) != 1 || !strings.Contains(res.Note, "manage_files") {
		t.Fatalf("a typo must come back with a did-you-mean: notFound=%v note=%q", res.NotFound, res.Note)
	}
}
