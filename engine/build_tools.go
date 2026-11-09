package engine

import (
	"strings"

	"openfabrica/tools"
)

// buildAllTools resolves the build's STATIC tool set ONCE: the deduped union of every
// plan item's needs (core + data-backbone when the plan does data work + planner-declared +
// keyword-inference). Up front is cache-correct: appending a schema mid-session reserialises
// the tool prefix the provider cache keyed on, so the whole prefix re-bills at full price.
// A single mid-session find_tool load can dominate a build's uncached input. find_tool
// still covers the unplanned. The backbone decision is taken once for the whole plan, not
// per task, so the resolved set is the same whichever task is examined first.
func buildAllTools(plan *Plan) []string {
	withBackbone := planNeedsBackbone(plan)
	if plan == nil || len(plan.Tasks) == 0 {
		return buildPreloadTools(Task{}, withBackbone)
	}
	var names []string
	declared := map[string]bool{}
	for _, t := range plan.Tasks {
		for _, n := range t.Tools {
			declared[strings.TrimSpace(n)] = true
		}
		names = append(names, buildPreloadTools(Task{Title: t.Title, Intent: t.Intent, Tools: t.Tools}, withBackbone)...)
	}
	if len(plan.ExternalLibraries) > 0 {
		// Declared libraries are registered before the build; the builds that touch them
		// afterwards do it to repair one (a CDN path that 404s, an unloaded font family),
		// which is a manage_libraries call. Preloading it avoids a cache-busting find_tool hop.
		names = append(names, "manage_libraries")
	}
	names = dedupeStrings(names)
	// A locked "no auth" decision rules out the auth tools keyword inference adds on a
	// phrase like "public, no-auth" (the hyphen reads as a word boundary, so "auth"
	// matches): a schema plus its auth_contract guide riding every turn of a build that
	// has no login. Planner-declared tools always stay.
	if vetoed := decisionVetoedTools(plan); len(vetoed) > 0 {
		kept := names[:0]
		for _, n := range names {
			if vetoed[n] && !declared[n] {
				continue
			}
			kept = append(kept, n)
		}
		names = kept
	}
	// A locked realtime-model decision settles the realtime wire tool, so a
	// keyword-inferred schema for another model ("multiplayer" infers all four) would
	// ride every turn for a model the plan ruled out; a locked "none" rules them all
	// out ("chat" in a chatbot task infers manage_relay). Planner-declared tools always stay.
	choice := planRealtimeChoice(plan.Decisions)
	if locked, ok := realtimeChoiceTool(choice); ok || choice == realtimeNone {
		kept := names[:0]
		for _, n := range names {
			if isRealtimeModelTool(n) && n != locked && !declared[n] {
				continue
			}
			kept = append(kept, n)
		}
		names = kept
	}
	return names
}

// authInferredTools are the tools keyword inference can add for an auth phrase.
var authInferredTools = []string{toolAuth, toolOAuth}

// decisionVetoedTools returns the inferable tools a locked plan decision rules out.
// Only KEYWORD inference is vetoed; a tool the planner declared is honoured regardless.
func decisionVetoedTools(plan *Plan) map[string]bool {
	if plan == nil {
		return nil
	}
	var out map[string]bool
	for _, d := range plan.Decisions {
		if isAuthTopic(d.Topic) && isNoAuthChoice(d.Choice) {
			if out == nil {
				out = map[string]bool{}
			}
			for _, n := range authInferredTools {
				out[n] = true
			}
		}
	}
	return out
}

// isAuthTopic reports whether a decision topic is the auth strategy ("auth strategy",
// "authentication", "login").
func isAuthTopic(topic string) bool {
	for _, tok := range decisionTokens(topic) {
		if tok == "auth" || tok == "authentication" || tok == "login" {
			return true
		}
	}
	return false
}

// isNoAuthChoice reports whether an auth-strategy choice means the project has no
// sign-in at all: "none", "no auth", "no login", "anonymous", "public".
func isNoAuthChoice(choice string) bool {
	toks := decisionTokens(choice)
	if len(toks) == 0 {
		return false
	}
	switch toks[0] {
	case "none", "no", "anonymous", "public":
		return true
	}
	return false
}

// planExtraTools is buildAllTools minus the always-loaded core and backbone: the tools
// this plan pulled in, the only ones whose routing guidance is worth per-project prompt
// bytes (the always-loaded set rides the byte-stable system prefix instead). Order follows
// buildAllTools, so the block is deterministic.
func planExtraTools(plan *Plan) []string {
	always := map[string]bool{}
	for _, n := range tools.CoreBuildToolNames {
		always[n] = true
	}
	for _, n := range tools.BuildBackboneToolNames {
		always[n] = true
	}
	var out []string
	for _, n := range buildAllTools(plan) {
		if !always[n] {
			out = append(out, n)
		}
	}
	return out
}

// buildPreloadTools assembles one plan item's STATIC tool set: core schemas, the
// data-backbone schemas when withBackbone says the plan does data work, the
// planner-declared task.Tools, and keyword inference, unioned and deduped.
// The whole set resolves BEFORE the session starts because appending a schema mid-segment
// invalidates the prompt-prefix cache. Inference is unioned in unconditionally on cost
// asymmetry, and the asymmetry is steep: a redundant name is ~2.4K bytes of schema read at
// the cache rate for the build (a few thousand token-equivalents), while a miss re-bills the
// whole prefix at full price plus the turn that asked. Preload generously.
// The backbone tier is the one exception, because it is three schemas that are re-read on
// every turn for the life of the build; see planNeedsBackbone.
func buildPreloadTools(task Task, withBackbone bool) []string {
	names := append([]string{}, tools.CoreBuildToolNames...)
	if withBackbone {
		names = append(names, tools.BuildBackboneToolNames...)
	}
	names = append(names, task.Tools...)
	names = append(names, inferTaskTools(task)...)
	for _, n := range names {
		names = append(names, impliedTools[n]...)
	}
	return dedupeStrings(names)
}

// impliedTools maps a tool onto the tools its work cannot be done without, preloaded
// alongside it. A hosted agent's tools ARE manage_function endpoints (manage_agent's
// `functions` CSV), so an agent task declaring only manage_agent sent the build to
// find_tool mid-segment and re-billed the cached prefix at full price.
var impliedTools = map[string][]string{
	"manage_agent": {"manage_function"},
	// A buyer opens checkout through a function endpoint's ctx.checkout, the provider
	// is registered first, and fulfilment is a payment.completed trigger: a payments
	// task cannot be built without all three.
	"manage_payments":      {"manage_function", "manage_providers", "manage_triggers"},
	"manage_subscriptions": {"manage_function", "manage_providers", "manage_triggers"},
}

// planNeedsBackbone reports whether a plan does plain data work, which is what the
// data-backbone tier (manage_schema / manage_api / manage_data) serves. The signal is a task
// that OWNS a bare resource name and declares no tool of its own: a bare name in `paths` is a
// table or its CRUD endpoint, as opposed to a page route (leading slash) or the shared
// layout (`default`). A realtime, signaling, upload, or LLM-proxy task also owns a bare
// resource name, but it declares the tool that creates that resource, so it does not count.
// The auth tools are the exception: they attach to an existing user table, so declaring one
// implies schema work. An explicit backbone name in task.Tools counts outright, and so does
// a tool-less task whose title or intent names a table.
//
// Worth a predicate because the tier is ~11KB of tool schema on the cached prefix,
// re-read EVERY turn for the life of the build, and a plan with no table or CRUD work
// never calls it. Three schemas carried for tens of turns is the one case that outweighs
// a find_tool hop; for a single tool the hop is far more expensive (see buildPreloadTools).
//
// A missing or empty plan keeps the tier: its shape is unknown, and the LIVE and scheduled
// paths have no plan to read at all.
func planNeedsBackbone(plan *Plan) bool {
	if plan == nil || len(plan.Tasks) == 0 {
		return true
	}
	backbone := map[string]bool{}
	for _, n := range tools.BuildBackboneToolNames {
		backbone[n] = true
	}
	// Tools that work over an EXISTING user table rather than creating their own
	// resource: declaring one implies manage_schema work first.
	backboneImplying := map[string]bool{toolAuth: true, toolOAuth: true}
	anyShape := false
	for _, t := range plan.Tasks {
		if len(t.Paths) > 0 || len(t.Edits) > 0 {
			anyShape = true
		}
		declaresOwnTool := false
		for _, n := range t.Tools {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if backbone[n] || backboneImplying[n] {
				return true
			}
			declaresOwnTool = true
		}
		// A task that declares its own tool usually owns that tool's resource (a relay
		// endpoint, an upload path), not a table - unless its text says it builds one too
		// ("messages table + relay"). The plan frame tells the planner not to list the
		// backbone, so that text is the only signal such a task gives.
		if declaresOwnTool && !mentionsTable(t) {
			continue
		}
		for _, p := range t.Paths {
			if isBareResourcePath(p) {
				return true
			}
		}
		// A table task the planner left without a bare resource in `paths` (no paths at
		// all, or only routes) still says what it builds in its text.
		if !declaresOwnTool && mentionsTable(t) {
			return true
		}
	}
	// No task declares paths or edits: the plan's shape is unknown, keep the tier.
	return !anyShape
}

// mentionsTable reports whether a plan task's title or intent names a table (whole word),
// the backbone signal for a task that also declares a non-backbone tool.
func mentionsTable(t PlanTaskSpec) bool {
	text := strings.ToLower(t.Title + " " + t.Intent)
	return containsWord(text, "table") || containsWord(text, "tables")
}

// isBareResourcePath reports whether a plan path names a table or endpoint resource rather
// than a page route or a module file. Routes and module files both carry a slash; `default`
// is the shared layout's canonical path and owns no resource.
func isBareResourcePath(p string) bool {
	p = strings.TrimSpace(p)
	return p != "" && p != "default" && !strings.Contains(p, "/")
}

// taskToolInference maps task-text keywords to the deferred tool the work almost certainly
// needs, unioned into EVERY task's preload set (a false positive costs one cached schema, a
// miss a cache-busting find_tool). The ONLY routing-keyword list in the engine, so keywords
// stay narrow and high-precision; wide terms belong in a guide's `use_when:` prose. Matching
// is whole-word (containsWord), so `register` cannot fire manage_auth on a callback registry.
var taskToolInference = []struct {
	tool     string
	keywords []string
}{
	{"manage_relay", []string{"websocket", "websockets", "realtime", "real time", "multiplayer", "chat", "presence"}},
	// "multiplayer" alone does not say which model, so it preloads all of them; a locked
	// realtime-model decision prunes the others (buildAllTools). The rest are narrow.
	{"manage_tick", []string{"multiplayer", "live cursors", "cursors", "live positions"}},
	{"manage_authoritative", []string{"multiplayer", "turn-based", "turn based", "anti-cheat", "auction", "auctions", "bidding"}},
	{"manage_matchmaking", []string{"multiplayer", "matchmaking", "find an opponent", "auto-match", "automatch"}},
	{"manage_signaling", []string{"webrtc", "signaling", "video call", "video calls", "voice call", "voice calls", "video chat"}},
	// AI endpoints. The natural-language cases (summarize/generate/classify) route via
	// the llm_endpoints use_when at PLAN; these are the high-precision tech terms that
	// must not miss. "chatbot" is a distinct word from the realtime "chat" above - the
	// whole-word match never fires "chat" inside "chatbot".
	{"manage_llm_proxy", []string{"chatbot", "chat bot", "ai assistant"}},
	// One-way SSE. The feed/ticker/dashboard phrasings route via the server_push
	// use_when; these are the unambiguous protocol names.
	{"manage_sse", []string{"server-sent", "server sent"}},
	// Bare "register" is deliberately absent: it is the generic programming verb
	// ("register a callback") as often as the auth one. `registration` and the sign-up/
	// login terms carry the signal instead. "account"/"accounts" are absent for the
	// same reason: "admin account for content" is not an auth feature.
	{"manage_auth", []string{"auth", "authentication", "login", "signup", "sign-up", "sign up", "registration"}},
	{"manage_payments", []string{"payment", "payments", "stripe", "checkout", "subscription", "subscriptions"}},
	{"manage_email", []string{"email", "emails", "newsletter", "newsletters"}},
	{"manage_uploads", []string{"upload", "uploads", "uploading", "file picker"}},
	{"manage_schedules", []string{"cron", "schedule", "schedules", "scheduled", "recurring job", "recurring jobs"}},
	// Imagery. LookupFull drops these when no image or stock provider is
	// configured, so on an install without one the entry is inert and the model
	// is steered to author SVG instead.
	{"manage_image_gen", []string{"illustration", "illustrations", "artwork", "hero image", "avatar", "avatars"}},
	{"manage_stockphotos", []string{"photo", "photos", "photography", "stock image", "stock images"}},
	{"manage_media", []string{"thumbnail", "thumbnails"}},
	{"manage_seo", []string{"seo", "meta tags", "meta description", "sitemap", "robots", "open graph", "structured data"}},
}

// wordByte reports whether b can be part of a word for boundary purposes.
// Underscore counts, so a snake_case identifier is one word.
func wordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// containsWord reports whether kw occurs in text delimited by non-word bytes on
// both sides, so `register` matches "register form" but not "registerHook".
func containsWord(text, kw string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], kw)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(kw)
		if (start == 0 || !wordByte(text[start-1])) && (end == len(text) || !wordByte(text[end])) {
			return true
		}
		i = start + 1
	}
}

// inferTaskTools scans a task's title+intent for the inference keywords.
func inferTaskTools(task Task) []string {
	text := strings.ToLower(task.Title + " " + task.Intent)
	var out []string
	for _, inf := range taskToolInference {
		for _, kw := range inf.keywords {
			if containsWord(text, kw) {
				out = append(out, inf.tool)
				break
			}
		}
	}
	return out
}
