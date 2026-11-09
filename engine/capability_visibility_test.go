package engine

import (
	"strings"
	"testing"

	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// noProviderCaps is an install with an LLM and nothing else: no image
// generation, no stock photos, no web search, no embeddings.
var noProviderCaps = llm.Capabilities{HasLLM: true, HasPDF: true, HasPush: true}

// TestDeadCapabilityHiddenEverywhere pins the one rule that keeps a build from
// hunting for a tool that cannot exist: a capability with no provider is absent
// from EVERY surface the model reads. A single surface that still names it is
// enough to start the search, and each search round costs a turn plus a full
// context re-read.
func TestDeadCapabilityHiddenEverywhere(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	reg := tools.DefaultRegistry()

	// The guide index. PLAN keeps the full list, so it is the surface to check.
	// Assembled through the production constructor, so an omission there (like
	// HiddenGuides once was) fails here instead of shipping.
	sys := pack.Assemble(planAssembleInput(ps, reg, noProviderCaps, nil)).System
	if strings.Contains(sys, "media_generation") {
		t.Error("the guide index still lists media_generation with no image or stock provider - that line is what sends the builder searching for a generator")
	}
	if strings.Contains(sys, "AI-generated") {
		t.Error("a category note still promises AI-generated imagery with no image provider configured")
	}

	// The PLAN picker and its fallback select from the pack list, so a dead guide
	// has to be gone from it before either runs.
	live := withoutDeadGuides(ps, reg, noProviderCaps)
	if strings.Contains(guideSelectionSystem(pack.Capabilities(live)), "media_generation") {
		t.Error("the PLAN picker catalogue still offers media_generation with no image or stock provider")
	}
	if unroutedGuideFallback(&Plan{}, live)["media_generation"] {
		t.Error("the unrouted PLAN fallback still selects media_generation with no image or stock provider")
	}

	// The tool surfaces. manage_media only transforms an image something else
	// produced, so it is dead here too and must not read as a way in.
	for _, dead := range []string{"manage_image_gen", "manage_stockphotos", "manage_media"} {
		if reg.ExplainUnavailable(dead, noProviderCaps, StageBuild) == "" {
			t.Errorf("%s reports as loadable with no image or stock provider", dead)
		}
		for name, surface := range map[string]string{
			"DeferredIndex": reg.DeferredIndex(nil, noProviderCaps, StageBuild),
			"Index":         reg.Index(noProviderCaps, StageBuild),
		} {
			if strings.Contains(surface, dead) {
				t.Errorf("%s still names %s with no provider behind it", name, dead)
			}
		}
	}

	// One image source revives the whole set, transform tool included.
	withStock := llm.Capabilities{HasLLM: true, HasStock: true}
	if reg.ExplainUnavailable("manage_media", withStock, StageBuild) != "" {
		t.Error("manage_media must be loadable once a stock provider can put an image in /files")
	}
	if h := capabilityDeadGuides(ps, reg, withStock); h["media_generation"] {
		t.Error("media_generation must reappear once any image source exists")
	}
}

// TestSearchMissNamesTheFallback pins that a search which cannot succeed says so
// and says what to do instead. Every field of the result is omitempty, so a miss
// that adds nothing serialises to a bare success, which reads as "try different
// words" and buys a reworded retry for the price of a whole turn.
func TestSearchMissNamesTheFallback(t *testing.T) {
	reg := tools.DefaultRegistry()

	gated := reg.GatedMatches("generate image", noProviderCaps, 0, StageBuild)
	if len(gated) == 0 {
		t.Fatal("a search for image generation must surface the gated tool that covers it, not nothing")
	}
	var reasons string
	for _, g := range gated {
		reasons += g.Reason
	}
	if !strings.Contains(reasons, "SVG") {
		t.Errorf("the reason must name what to build instead, got: %s", reasons)
	}

	// Nothing is gated when everything is configured, so the same search falls
	// through to the generic verdict rather than inventing an obstacle.
	allCaps := llm.Capabilities{HasLLM: true, HasImage: true, HasStock: true, HasSearch: true,
		HasEmbeddings: true, HasPush: true, HasPDF: true}
	if got := reg.GatedMatches("generate image", allCaps, 0, StageBuild); len(got) != 0 {
		t.Errorf("nothing should be reported gated when every provider exists, got %v", got)
	}
	if got := reg.MissingCapabilities(allCaps, StageBuild); len(got) != 0 {
		t.Errorf("no capability should be missing when every provider exists, got %v", got)
	}
	missing := reg.MissingCapabilities(noProviderCaps, StageBuild)
	if len(missing) == 0 {
		t.Fatal("an install without image, stock, search or embeddings must report those as missing")
	}
	for _, m := range missing {
		if strings.TrimSpace(m.Reason) == "" {
			t.Errorf("capability %q has no fallback to offer", m.Name)
		}
	}
}

// TestRepeatFindQueryDetected pins the paraphrase guard. A fruitless hunt does
// not repeat a string, it rewords the same question, so equality would never
// catch it.
func TestRepeatFindQueryDetected(t *testing.T) {
	st := &segState{}
	st.recordFindQuery("generate AI images media")

	for _, q := range []string{
		"generate AI image",
		"image generate",
		"generate image from prompt AI stock photo",
	} {
		if st.repeatFindQuery(q) == "" {
			t.Errorf("%q restates the first search and should be answered from it", q)
		}
	}
	for _, q := range []string{
		"websocket relay room",
		"send transactional email",
		// One shared term is a shared subject, not the same question: refusing
		// this shape denied tools that exist (e.g. "keyword search" after a
		// "web search" miss).
		"media library search",
	} {
		if prior := st.repeatFindQuery(q); prior != "" {
			t.Errorf("%q is a different question but matched %q", q, prior)
		}
	}
}

// TestCapabilityFallbackCoversEveryTag keeps the advice complete: a tag any tool
// can declare must have something to say, or a build meets a dead end with no
// way out of it.
func TestCapabilityFallbackCoversEveryTag(t *testing.T) {
	for tag := range llm.KnownCapabilities {
		if strings.TrimSpace(tools.CapabilityFallback(tag)) == "" {
			t.Errorf("capability %q has no fallback text", tag)
		}
	}
}
