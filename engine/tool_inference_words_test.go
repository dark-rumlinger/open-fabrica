package engine

import "testing"

// Task prose is full of identifiers, so a substring scan over the inference
// keywords misroutes on them. The split architecture the plan frame teaches names
// a callback registry (registerHook/emitHook) on the zero-import state leaf, which
// a substring `register` reads as auth - pulling manage_auth's schema and the
// auth_contract guide into every multi-leaf build that has no auth at all. These
// pin the boundary rule and the recall it must not cost.

func TestInferTaskToolsIgnoresIdentifierSubstrings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		title  string
		intent string
	}{
		{"camelCase registry", "Zero-import shared state", "export SHARED_STATE and the registerHook(name, fn) / emitHook(name, payload) helpers"},
		{"snake_case registry", "State leaf", "expose register_hook and emit_hook for inbound callbacks"},
		{"registry noun", "App state", "a hooks registry for cross-leaf callbacks"},
		// "register" as the generic programming verb, which no boundary rule can
		// tell from the auth one - hence its absence from the table.
		{"register verb", "Networking leaf", "register a 'sendChat' hook callback so the world leaf can reach it"},
		{"register hooks", "World leaf", "register hooks for playerJoined/playerLeft/playerMoved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, got := range inferTaskTools(Task{Title: tc.title, Intent: tc.intent}) {
				if got == "manage_auth" {
					t.Errorf("inferred manage_auth from %q / %q - no auth surface is implied here", tc.title, tc.intent)
				}
			}
		})
	}
}

func TestInferTaskToolsStillMatchesRealWords(t *testing.T) {
	for _, tc := range []struct {
		intent string
		want   string
	}{
		{"user registration with email confirmation", "manage_auth"},
		{"a signup page posting to the auth endpoint", "manage_auth"},
		{"accounts page behind a login", "manage_auth"},
		{"authentication for the admin area", "manage_auth"},
		{"multiplayer presence over a websocket", "manage_relay"},
		{"websockets carry the chat lane", "manage_relay"},
		{"stripe checkout for subscriptions", "manage_payments"},
		{"uploading an avatar image", "manage_uploads"},
		{"a scheduled nightly digest", "manage_schedules"},
		{"a chatbot answering product questions", "manage_llm_proxy"},
		{"an ai assistant in the sidebar", "manage_llm_proxy"},
		{"a live price ticker over server-sent events", "manage_sse"},
		{"SEO meta per page, robots and a sitemap", "manage_seo"},
		{"open graph tags for every public page", "manage_seo"},
	} {
		t.Run(tc.want+": "+tc.intent, func(t *testing.T) {
			found := false
			for _, got := range inferTaskTools(Task{Intent: tc.intent}) {
				if got == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("intent %q did not infer %q", tc.intent, tc.want)
			}
		})
	}
}

// "chatbot" is an LLM signal distinct from the realtime "chat" keyword. Whole-word
// matching must keep "chat" from firing inside "chatbot", so a chatbot brief pulls
// the AI endpoint, not the multiplayer relay - the collision the two keywords risk.
func TestInferTaskToolsChatbotRoutesToLLMNotRelay(t *testing.T) {
	hasLLM, hasRelay := false, false
	for _, got := range inferTaskTools(Task{Intent: "build a chatbot for support"}) {
		switch got {
		case "manage_llm_proxy":
			hasLLM = true
		case "manage_relay":
			hasRelay = true
		}
	}
	if !hasLLM {
		t.Error("a chatbot brief must infer manage_llm_proxy")
	}
	if hasRelay {
		t.Error(`"chatbot" must not fire the realtime "chat" keyword (manage_relay); whole-word matching should prevent the collision`)
	}
}

func TestContainsWordBoundaries(t *testing.T) {
	for _, tc := range []struct {
		text, kw string
		want     bool
	}{
		{"registerhook(name, fn)", "register", false},
		{"register_hook", "register", false},
		{"registry of hooks", "register", false},
		{"the register form", "register", true},
		{"register", "register", true},
		{"(register)", "register", true},
		{"registerhook and register form", "register", true}, // a later real hit still counts
		{"sign up flow", "sign up", true},
		{"resign uphill", "sign up", false},
	} {
		if got := containsWord(tc.text, tc.kw); got != tc.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", tc.text, tc.kw, got, tc.want)
		}
	}
}

// A plan that declares external libraries preloads manage_libraries: the builds that touch a
// declared library afterwards do it to repair one, and a mid-build find_tool load re-bills the
// whole cached prefix.
func TestBuildAllToolsPreloadsLibrariesWhenDeclared(t *testing.T) {
	plan := &Plan{Goal: "a viewer", Tasks: []PlanTaskSpec{
		{ID: "home", Title: "Home page", Intent: "landing page", Paths: []string{"/"}},
	}}
	for _, n := range buildAllTools(plan) {
		if n == "manage_libraries" {
			t.Fatalf("manage_libraries preloaded for a plan with no external libraries")
		}
	}
	plan.ExternalLibraries = []LibrarySpec{{Name: "qrcode", URL: "https://cdn.example.com/qrcode.min.js"}}
	found := false
	for _, n := range buildAllTools(plan) {
		if n == "manage_libraries" {
			found = true
		}
	}
	if !found {
		t.Errorf("manage_libraries not preloaded for a plan that declares external libraries")
	}
}
