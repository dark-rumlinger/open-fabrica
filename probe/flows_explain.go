package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"openfabrica/internal/stringutil"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Failure explanation: turns a failed step plus page state into the owner-facing
// message, and collects the hard console errors behind it.

// hardErrors drains every open session's errors since the previous step and returns
// the ones that break the app for a real user, classified exactly like the load
// probe. A step in one session can crash another (a chat message that throws in the
// recipient's tab), so all sessions are checked; with several, each line is tagged.
func (r *flowRun) hardErrors() []string {
	var out []string
	for _, name := range r.order {
		s := r.sessions[name]
		collected := filterFlowNoise(s.errs.take())
		kept := collected[:0]
		for _, e := range collected {
			// A navigation the step itself caused cancels in-flight requests; Chrome
			// reports those as failed loads, but nothing is broken.
			if strings.Contains(e, "net::ERR_ABORTED") {
				continue
			}
			// A request that fails because an offline step took the network down is
			// the connection, not the app (through a service worker it surfaces as a
			// plain failed load).
			if strings.Contains(e, "net::ERR_INTERNET_DISCONNECTED") || (s.offline && strings.HasPrefix(e, "[load_failed")) {
				continue
			}
			kept = append(kept, e)
		}
		hard, soft := splitHardSoft(kept)
		r.noteScriptFails(soft)
		var violations []string
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		_ = chromedp.Run(ctx, chromedp.Evaluate(
			`(() => { const v = window.__ofProbeViolations || []; const out = v.slice(); v.length = 0; return out; })()`,
			&violations))
		cancel()
		for _, v := range violations {
			hard = append(hard, "[envelope_misuse] "+v)
		}
		for _, h := range hard {
			if len(r.sessions) > 1 {
				h = "(" + name + ") " + h
			}
			out = append(out, h)
		}
	}
	return out
}

// filterFlowNoise drops the environmental noise the load probe drops, except
// flowRealErrors.
func filterFlowNoise(errs []string) []string {
	var out []string
	for _, e := range errs {
		lower := strings.ToLower(e)
		real := slices.ContainsFunc(flowRealErrors, func(p string) bool { return strings.Contains(lower, p) })
		if real || len(filterEnvironmentalNoise([]string{e})) == 1 {
			out = append(out, e)
		}
	}
	return out
}

// noteScriptFails keeps the cross-origin script load failures among soft errors,
// deduplicated and capped, for the failure explanation.
func (r *flowRun) noteScriptFails(soft []string) {
	for _, e := range soft {
		if !strings.HasPrefix(e, crossOriginScriptPrefix) || len(r.scriptFails) >= 3 {
			continue
		}
		e = strings.TrimPrefix(e, "[load_failed_asset] ")
		if !slices.Contains(r.scriptFails, e) {
			r.scriptFails = append(r.scriptFails, e)
		}
	}
}

// explain describes a failed step so its cause is visible without guesswork: where
// the page was, what happened to the step's target, which requests failed (with the
// server's answer), which controls were on screen, and a one-line verdict on whether
// the app or the script is the likely culprit. Best effort: anything it cannot read
// is left out.
func (r *flowRun) explain(s *flowSession, st FlowStep, fail flowFailure, hard []string, fails []httpFail) string {
	if s == nil {
		return ""
	}
	sel := r.vars.expand(stepSelector(st), s.name)
	scope := r.vars.expand(st.In, s.name)
	verb, arg := st.verb()
	want := ""
	if verb == "expectText" || verb == "expectNoText" {
		want = r.vars.expand(arg, s.name)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	var page flowDOMResult
	args, _ := json.Marshal([]any{"context", sel, "", scope, map[string]string{"verb": verb, "want": want}})
	_ = chromedp.Run(ctx, chromedp.Evaluate("("+flowDOMJS+").apply(null, "+string(args)+")", &page))

	var b strings.Builder
	if page.URL != "" {
		fmt.Fprintf(&b, "Page at failure: %s", page.URL)
		if page.Title != "" {
			fmt.Fprintf(&b, " (%q)", page.Title)
		}
		b.WriteString("\n")
	}
	if sel != "" && page.Target != "" && page.Target != "visible" {
		fmt.Fprintf(&b, "Target %q: %s\n", sel, page.Target)
	}
	r.explainSeen(&b, page, verb, sel, scope, want)
	has4xx, has5xx, has429 := false, false, false
	if len(fails) > 0 {
		b.WriteString("Requests that failed in the last steps:\n")
		for i, f := range fails {
			if f.status >= 500 {
				has5xx = true
			} else {
				has4xx = true
				has429 = has429 || f.status == 429
			}
			if i >= 3 {
				continue
			}
			path := f.url
			if u, err := url.Parse(f.url); err == nil {
				path = u.RequestURI()
			}
			fmt.Fprintf(&b, "  - step %d: %s %s -> %d", f.step+1, f.method, path, f.status)
			if body := r.responseBody(f); body != "" {
				fmt.Fprintf(&b, ": %s", body)
			}
			b.WriteString("\n")
		}
	}
	r.explainCalls(&b)
	if len(r.scriptFails) > 0 {
		b.WriteString("Scripts that failed to load from another origin:\n")
		for _, f := range r.scriptFails {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if len(page.Controls) > 0 {
		line := stringutil.Truncate(strings.Join(page.Controls, " | "), 3000)
		fmt.Fprintf(&b, "Visible controls (usable selectors): %s\n", line)
	}
	if len(page.Closest) > 0 {
		fmt.Fprintf(&b, "Closest to %q: %s\n", sel, strings.Join(page.Closest, ", "))
	}
	if len(page.TextMatches) > 0 {
		fmt.Fprintf(&b, "Did you mean %s? text= matches an element's WHOLE visible label, not part of it.\n", strings.Join(page.TextMatches, " or "))
	}
	switch {
	case fail.kind == "covered":
		b.WriteString("Verdict: another element sits on top of the target, so a real user could not click it either. Fix the app: hide the covering element properly (a rule such as display:flex overrides the hidden attribute, so add [hidden]{display:none} or toggle a class instead), or give it pointer-events:none. Do not change the step.")
	case len(hard) > 0 || has5xx:
		b.WriteString("Verdict: the app errored (see above). Fix the app code; the steps themselves are probably right.")
	case len(r.scriptFails) > 0 && (page.Target == "missing" || fail.kind == "assert"):
		b.WriteString("Verdict: a script the page imports failed to load in the test browser (see above), so the page's JS never ran - that is why the target is missing. If it is a declared CDN library, this is the test environment, not your code: do NOT restructure the app around it (no fallbacks, no dynamic imports, no static copies of JS-built markup); report it and move on. If it is a URL you wrote, fix the URL.")
	case has429:
		// Each flow session is rate-limited as its own visitor (replayid), so this is
		// what one real person walking the journey would get - not replay pile-up.
		b.WriteString("Verdict: the app's own rate limit refused this visitor (429). Each flow session is counted as a separate visitor with a fresh limit, so ONE real person doing this journey hits it too: the page requests that endpoint too often (a poll, a retry loop, a re-fetch on every render) - make it fetch less. Raise the endpoint's rate limit only if one real person genuinely needs that many requests a minute; never raise it just to pass the replay.")
	case has4xx:
		b.WriteString("Verdict: a request was refused (4xx). Often the flow is not signed in (every flow starts logged out), lacks a permission, or sends data the endpoint rejects; the response above says which.")
	case verb == "expectText" && scope != "" && len(page.FoundIn) > 0 && !page.FoundInScope:
		b.WriteString("Verdict: the text is on the page, but not inside the in: element (compare the two lines above). Either the app put it in the wrong place or state - an item in the wrong column, a status that never changed - which is the bug to fix, or in: names the wrong element.")
	case verb == "expectText" && len(page.FoundIn) > 0:
		b.WriteString("Verdict: the text is there now, so it appeared only after the step gave up. The app is slow here rather than broken: give this step a longer timeout (or wait for what signals the work finished).")
	case verb == "expectText" && page.Near != "":
		b.WriteString("Verdict: the page shows a different version of this text (see the closest match above). If the earlier steps explain the difference (a name shortened or a number reformatted by the app's own design), fix the step; otherwise fix the app. Never change security, sign-in or access settings to make a step pass.")
	case verb == "expectNoText" && len(page.FoundIn) > 0:
		b.WriteString("Verdict: the element above still shows it. Either the app never removed or updated it (the change did not reach the page - the bug to fix), or the text also appears somewhere the step did not mean (a toast, a log, another record): then scope the step with in:.")
	case page.Target == "missing":
		b.WriteString("Verdict: nothing errored and the target is not on the page. Either the step does not match the UI (use a selector from the visible controls above) or the app never rendered it, which is then the bug.")
	case page.Target == "hidden":
		b.WriteString("Verdict: the target exists but is hidden. Reveal it the way a user would first (open the menu, tab or dialog), or target the visible control that stands in for it.")
	case page.Target == "disabled":
		b.WriteString("Verdict: the target is disabled, so a user could not use it yet; check what is supposed to enable it.")
	case page.Target == "invalid selector":
		b.WriteString("Verdict: the step's selector is not valid CSS; fix the step.")
	case fail.kind == "assert":
		b.WriteString("Verdict: the page responded, but not as the step expected (see the page above). If the app is wrong fix the app; if the expectation is wrong fix the step. Never change security, sign-in or access settings to make a step pass, and do not turn of_test.js into diagnostics: read the code instead.")
	default:
		b.WriteString("Verdict: if the journey is right, fix the app code; if the UI deliberately changed, re-save of_test.js with manage_testing action=write_flows.")
	}
	return b.String()
}

// explainSeen writes what the page showed where the step looked: the in: elements'
// text, where the wanted text is instead (or still is), the closest text when it is
// nowhere, the elements a count or visibility step matched, and the earlier steps'
// notes. Without these a red text step only says what is missing, and the fixer is
// left guessing between a broken app and a wrong script.
func (r *flowRun) explainSeen(b *strings.Builder, page flowDOMResult, verb, sel, scope, want string) {
	if scope != "" {
		switch page.ScopeState {
		case "visible":
			fmt.Fprintf(b, "Inside %q (%d visible) the page shows: %s\n", scope, page.ScopeCount, strings.Join(page.Scope, " | "))
		case "":
		default:
			fmt.Fprintf(b, "Scope in %q: %s\n", scope, page.ScopeState)
		}
	}
	if want != "" {
		switch {
		case verb == "expectText" && len(page.FoundIn) > 0 && scope != "" && !page.FoundInScope:
			fmt.Fprintf(b, "%q IS on the page, outside %q: in %s\n", want, scope, strings.Join(page.FoundIn, " | "))
		case verb == "expectText" && len(page.FoundIn) > 0:
			fmt.Fprintf(b, "%q is on the page now: in %s\n", want, strings.Join(page.FoundIn, " | "))
		case verb == "expectText" && page.HiddenIn != "":
			fmt.Fprintf(b, "%q is only in a hidden element (%s): the app rendered it but does not show it\n", want, page.HiddenIn)
		case verb == "expectText" && page.Near != "":
			fmt.Fprintf(b, "%q is nowhere on the page; the closest text is in %s\n", want, page.Near)
		case verb == "expectText":
			fmt.Fprintf(b, "%q is nowhere on the page, not even in part\n", want)
		case verb == "expectNoText" && len(page.FoundIn) > 0:
			fmt.Fprintf(b, "%q is still shown in: %s\n", want, strings.Join(page.FoundIn, " | "))
		}
	}
	if sel != "" && (page.MatchCount > 0 || page.HiddenCount > 0) {
		fmt.Fprintf(b, "%q matches %d visible and %d hidden elements", sel, page.MatchCount, page.HiddenCount)
		if len(page.Matches) > 0 {
			fmt.Fprintf(b, ": %s", strings.Join(page.Matches, " | "))
		}
		b.WriteString("\n")
	}
	if len(r.notes) > 0 {
		fmt.Fprintf(b, "Earlier steps did not go exactly as written:\n  - %s\n", strings.Join(r.notes, "\n  - "))
	}
	for _, name := range r.order {
		if msg := r.sessions[name].dialog.lastMessage(); msg != "" {
			fmt.Fprintf(b, "Last dialog in %s: %s\n", name, stringutil.Truncate(msg, 200))
		}
	}
}

// explainCalls writes the newest API responses of the last steps that did not fail
// (the failed ones are listed already), each with the start of its body. A page that
// reads a field the response never carries fails silently, and the response is the
// only place that shows it.
func (r *flowRun) explainCalls(b *strings.Builder) {
	var ok []httpFail
	for _, c := range r.recentCalls {
		if c.status < 400 {
			ok = append(ok, c)
		}
	}
	if len(ok) > maxExplainedCalls {
		ok = ok[len(ok)-maxExplainedCalls:]
	}
	if len(ok) == 0 {
		return
	}
	b.WriteString("API responses in the last steps (what the page received):\n")
	for _, c := range ok {
		path := c.url
		if u, err := url.Parse(c.url); err == nil {
			path = u.RequestURI()
		}
		fmt.Fprintf(b, "  - step %d: %s %s -> %d", c.step+1, c.method, path, c.status)
		if body := r.responseBody(c); body != "" {
			fmt.Fprintf(b, ": %s", body)
		}
		b.WriteString("\n")
	}
}

// responseBody returns the start of a response's body, whitespace collapsed.
func (r *flowRun) responseBody(f httpFail) string {
	s := r.sessions[f.session]
	if s == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	var body []byte
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		body, err = network.GetResponseBody(f.id).Do(ctx)
		return err
	})); err != nil {
		return ""
	}
	return stringutil.Truncate(strings.Join(strings.Fields(string(body)), " "), 200)
}

// consoleTail renders the newest console lines of every open session, tagged with the
// session when there are several. Empty when the app logged nothing.
func (r *flowRun) consoleTail() string {
	var lines []string
	for _, name := range r.order {
		s := r.sessions[name]
		if s == nil || s.errs == nil {
			continue
		}
		for _, l := range s.errs.recentLogs(maxFlowConsoleLines) {
			if len(r.sessions) > 1 {
				l = "(" + name + ") " + l
			}
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n  ")
}
