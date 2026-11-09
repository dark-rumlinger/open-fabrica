package probe

// Forgiving input for of_test.js. The model writes flows from habits formed on
// other test tools (Playwright, Cypress) and from memory of the page, so the same
// unambiguous slips recur on every kind of project. Rejecting each one costs a round
// trip and teaches nothing; instead every slip whose intent is clear is rewritten to
// the canonical step before strict validation, and the save reports what changed.
// Anything genuinely ambiguous is left alone for validation to reject with a reason.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// flowKeyAliases maps step keys borrowed from other test tools to this format's.
var flowKeyAliases = map[string]string{
	// verbs
	"tap": "click", "doubleClick": "dblclick", "dblClick": "dblclick", "doubleclick": "dblclick",
	"sendKeys": "type", "pressSequentially": "type", "typeText": "type",
	"visit": "goto", "navigate": "goto", "open": "goto",
	"sleep": "wait", "pause": "wait",
	"expectURL": "expectUrl", "expectPath": "expectUrl",
	"assertText": "expectText", "seeText": "expectText", "expectContains": "expectText",
	"assertNoText":    "expectNoText",
	"waitForSelector": "waitFor", "waitForElement": "waitFor",
	"expectJS": "expectJs", "uploadFile": "upload", "attach": "upload",
	"refresh": "reload",
	// qualifiers
	"attribute": "attr", "within": "in", "inside": "in", "scope": "in", "container": "in",
}

// textAttrAliases are DOM properties a remember step sometimes names as attr; the
// step already reads an element's text (or a form field's value) without attr.
var textAttrAliases = map[string]bool{"textContent": true, "innerText": true, "text": true, "innerHTML": true, "value": true}

// pressKeyAliases maps key spellings from KeyboardEvent.code/key and other tools
// onto press's key names. KeyA..KeyZ and Digit0..Digit9 are handled by pattern.
var pressKeyAliases = map[string]string{
	"Esc": "Escape", "Return": "Enter", "Spacebar": "Space", "Del": "Delete",
	"Up": "ArrowUp", "Down": "ArrowDown", "Left": "ArrowLeft", "Right": "ArrowRight",
	"ShiftLeft": "Shift", "ShiftRight": "Shift", "ControlLeft": "Control", "ControlRight": "Control",
	"Ctrl": "Control", "AltLeft": "Alt", "AltRight": "Alt", "Option": "Alt",
	"MetaLeft": "Meta", "MetaRight": "Meta", "Cmd": "Meta", "Command": "Meta",
}

// normalizePressKey rewrites the key of a press argument (after any "Mod+" chord
// prefix) from a code-style spelling to the one press takes: "KeyW" -> "w",
// "Digit1" -> "1", "Esc" -> "Escape". It returns the argument unchanged otherwise.
func normalizePressKey(arg string) string {
	prefix, key := "", arg
	if i := strings.LastIndex(arg, "+"); i > 0 && i < len(arg)-1 {
		prefix, key = arg[:i+1], arg[i+1:]
	}
	switch {
	case len(key) == 4 && strings.HasPrefix(key, "Key") && key[3] >= 'A' && key[3] <= 'Z':
		key = strings.ToLower(key[3:])
	case len(key) == 6 && strings.HasPrefix(key, "Digit") && key[5] >= '0' && key[5] <= '9':
		key = key[5:]
	default:
		if k, ok := pressKeyAliases[key]; ok {
			key = k
		}
	}
	return prefix + key
}

// numericKeys are step keys that take numbers.
var numericKeys = []string{"wait", "timeout", "count", "hold"}

func looksLikeSelector(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "#") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "[") || strings.HasPrefix(s, "text=")
}

// normalizeFlowJSON rewrites unambiguous slips in the evaluated of_test.js default
// export and returns the canonical JSON plus one note per rewrite. Input it cannot
// read as { flows: [...] } is returned unchanged, for strict validation to explain.
func normalizeFlowJSON(raw []byte) ([]byte, []string) {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return raw, nil
	}
	flows, ok := doc["flows"].([]any)
	if !ok {
		return raw, nil
	}
	var notes []string
	for fi, fv := range flows {
		flow, ok := fv.(map[string]any)
		if !ok {
			continue
		}
		name, _ := flow["name"].(string)
		if name == "" {
			name = fmt.Sprintf("#%d", fi+1)
		}
		steps, _ := flow["steps"].([]any)
		if n := applyFlowSession(flow, steps); n != "" {
			notes = append(notes, fmt.Sprintf("flow %q: %s", name, n))
		}
		if split, n := splitNavigateThen(steps); n != "" {
			steps = split
			flow["steps"] = split
			notes = append(notes, fmt.Sprintf("flow %q: %s", name, n))
		}
		if kept, n := expandSessionMarkers(steps); n != "" {
			steps = kept
			flow["steps"] = kept
			notes = append(notes, fmt.Sprintf("flow %q: %s", name, n))
		}
		if n := dropOpeningOnlySession(steps); n != "" {
			notes = append(notes, n)
		}
		if n := inheritOpeningSession(steps); n != "" {
			notes = append(notes, fmt.Sprintf("flow %q: %s", name, n))
		}
		for si, sv := range steps {
			step, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			for _, n := range normalizeStep(step) {
				notes = append(notes, fmt.Sprintf("flow %q step %d: %s", name, si+1, n))
			}
		}
		if withSnaps, n := insertMissingSnapshots(steps); n != "" {
			flow["steps"] = withSnaps
			notes = append(notes, fmt.Sprintf("flow %q: %s", name, n))
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return raw, nil
	}
	return out, notes
}

func normalizeStep(step map[string]any) []string {
	var notes []string
	// Borrowed key names, unless the canonical key is also present (then the step
	// is ambiguous and validation rejects it with a reason).
	keys := make([]string, 0, len(step))
	for k := range step {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		canon, ok := flowKeyAliases[k]
		if !ok {
			continue
		}
		if _, clash := step[canon]; clash {
			continue
		}
		step[canon] = step[k]
		delete(step, k)
		notes = append(notes, fmt.Sprintf("%s read as %s", k, canon))
	}

	// Playwright selector habits: a quoted text= label, and "outer >> inner" chains,
	// which say "the inner control inside the outer element" - exactly in: plus the
	// step's own selector.
	for _, k := range selectorStepKeys {
		sel, ok := step[k].(string)
		if !ok {
			continue
		}
		if uq, changed := unquoteTextSelector(sel); changed {
			step[k] = uq
			notes = append(notes, fmt.Sprintf("%s %q read as %q (text= takes the label without quotes)", k, sel, uq))
			sel = uq
		}
		parts := strings.Split(sel, " >> ")
		if len(parts) != 2 || k == "in" || k == "to" || k == "from" || !qualifierVerbs["in"][k] {
			continue
		}
		if _, hasIn := step["in"]; hasIn {
			continue
		}
		inner, _ := unquoteTextSelector(strings.TrimSpace(parts[1]))
		step["in"], step[k] = strings.TrimSpace(parts[0]), inner
		notes = append(notes, fmt.Sprintf("%s %q read as {%s: %q, in: %q}", k, sel, k, inner, step["in"]))
	}

	// {expectText: "#column", value: "Sarah"} (or text/contains): the verb names
	// where to look and the extra key what to find, which is exactly in: plus text.
	for _, verb := range []string{"expectText", "expectNoText"} {
		sel, ok := step[verb].(string)
		if !ok || !looksLikeSelector(sel) {
			continue
		}
		if _, hasIn := step["in"]; hasIn {
			continue
		}
		for _, k := range []string{"value", "text", "contains"} {
			if want, ok := step[k].(string); ok && want != "" {
				step["in"], step[verb] = sel, want
				delete(step, k)
				notes = append(notes, fmt.Sprintf("{%s: %q, %s: %q} read as {%s: %q, in: %q}", verb, sel, k, want, verb, want, sel))
				break
			}
		}
	}

	// fill or type with text instead of value.
	for _, verb := range []string{"fill", "type"} {
		if _, isVerb := step[verb]; !isVerb {
			continue
		}
		if _, hasValue := step["value"]; !hasValue {
			if t, ok := step["text"]; ok {
				step["value"] = t
				delete(step, "text")
				notes = append(notes, verb+" text read as value")
			}
		}
	}

	// expectCount with value/equals instead of count.
	if _, isCount := step["expectCount"]; isCount {
		if _, hasCount := step["count"]; !hasCount {
			for _, k := range []string{"value", "equals"} {
				if v, ok := step[k]; ok {
					step["count"] = v
					delete(step, k)
					notes = append(notes, fmt.Sprintf("expectCount %s read as count", k))
					break
				}
			}
		}
	}

	// remember with attr naming a DOM property: the step reads text (or a field's
	// value) by default, which is what the property would have given.
	if _, isRemember := step["remember"]; isRemember {
		if a, ok := step["attr"].(string); ok && textAttrAliases[a] {
			delete(step, "attr")
			notes = append(notes, fmt.Sprintf("remember attr %q dropped: without attr it reads the element's text (or a field's value)", a))
		}
	}

	// Numbers written as strings.
	for _, k := range numericKeys {
		if sv, ok := step[k].(string); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sv), "ms"))); err == nil {
				step[k] = n
				notes = append(notes, fmt.Sprintf("%s %q read as %d", k, sv, n))
			}
		}
	}

	// wait / waitFor mixed up: wait is a number of milliseconds, waitFor an element
	// selector, and the two names swap easily. A number in waitFor can only be a
	// delay, and a selector-shaped string in wait can only be an element to wait for,
	// so each moves to the verb it belongs to (unless that verb is already set).
	if v, ok := step["waitFor"]; ok {
		if _, isStr := v.(string); !isStr {
			if _, taken := step["wait"]; !taken {
				step["wait"] = v
				delete(step, "waitFor")
				notes = append(notes, fmt.Sprintf("waitFor %v read as wait (waitFor takes a selector; a bare number is a millisecond delay, which is the wait verb)", v))
			}
		}
	}
	if sel, ok := step["wait"].(string); ok && looksLikeSelector(sel) {
		if _, taken := step["waitFor"]; !taken {
			step["waitFor"] = sel
			delete(step, "wait")
			notes = append(notes, fmt.Sprintf("wait %q read as waitFor (wait takes milliseconds; a selector is what waitFor waits for)", sel))
		}
	}

	// press with a code-style key ("KeyW", "Digit1", "Esc").
	if p, ok := step["press"].(string); ok {
		if np := normalizePressKey(p); np != p {
			step["press"] = np
			notes = append(notes, fmt.Sprintf("press %q read as %q", p, np))
		}
	}

	// reload written as {reload: "yes"} / {reload: 1}: any value means reload.
	if v, ok := step["reload"]; ok {
		if _, isBool := v.(bool); !isBool {
			step["reload"] = true
			notes = append(notes, "reload read as reload: true")
		}
	}

	// A fixed wait longer than the cap: the intent (let slow work finish) is clear,
	// but a fixed sleep is the wrong tool for it - cap it and say what to use.
	if n, ok := step["wait"].(float64); ok && n > float64(maxFlowWait.Milliseconds()) {
		step["wait"] = maxFlowWait.Milliseconds()
		notes = append(notes, fmt.Sprintf("wait %v capped at %d ms; to wait for slow work, use waitFor/expectText with a timeout instead of a fixed sleep", n, maxFlowWait.Milliseconds()))
	} else if n, ok := step["wait"].(int); ok && int64(n) > maxFlowWait.Milliseconds() {
		step["wait"] = maxFlowWait.Milliseconds()
		notes = append(notes, fmt.Sprintf("wait %d capped at %d ms; to wait for slow work, use waitFor/expectText with a timeout instead of a fixed sleep", n, maxFlowWait.Milliseconds()))
	}

	// A session name in camelCase or with spaces ("boardWatcher"): lowercased with
	// the invalid characters dropped, the same way in every step, so the flow's
	// sessions still line up.
	if as, ok := step["as"].(string); ok && !flowSessionRe.MatchString(as) {
		if na := normalizeSessionName(as); na != "" {
			step["as"] = na
			notes = append(notes, fmt.Sprintf("as %q read as %q", as, na))
		}
	}
	return notes
}

// splitNavigateThen handles a goto (or reload) packed with ONE wait or assertion into a
// single step ({goto: "/", expectVisible: "#onboarding"}): "open the page,
// then check it" is the only order that means anything, so it becomes two steps. An
// action beside the goto ({click, goto}) is left for validation to reject. `as` goes on
// both (they are the same user); every other key belongs to the second verb. A step with
// more verbs than that is left for validation to reject. Returns the rewritten steps and
// a note, or the steps unchanged and "".
func splitNavigateThen(steps []any) ([]any, string) {
	verbs := map[string]bool{}
	for _, v := range verbNames() {
		verbs[v] = true
	}
	out := make([]any, 0, len(steps))
	n := 0
	for _, sv := range steps {
		st, ok := sv.(map[string]any)
		if !ok {
			out = append(out, sv)
			continue
		}
		nav, other := "", ""
		count := 0
		for k := range st {
			if !verbs[k] {
				continue
			}
			count++
			if (k == "goto" || k == "reload") && nav == "" {
				nav = k
			} else {
				other = k
			}
		}
		if count != 2 || nav == "" || !(other == "wait" || other == "waitFor" || strings.HasPrefix(other, "expect")) {
			out = append(out, sv)
			continue
		}
		first := map[string]any{nav: st[nav]}
		if as, ok := st["as"]; ok {
			first["as"] = as
		}
		second := map[string]any{}
		for k, v := range st {
			if k != nav {
				second[k] = v
			}
		}
		out = append(out, first, second)
		n++
	}
	if n == 0 {
		return steps, ""
	}
	return out, fmt.Sprintf("%d step(s) holding a goto plus another verb were split into the goto and then that verb (one verb per step)", n)
}

// applyFlowSession handles as written on the flow itself ({name, as: "bob", steps}):
// every step of that flow runs as that user, so the as moves onto each step that
// names no session of its own. Returns the note, or "" when nothing changed.
func applyFlowSession(flow map[string]any, steps []any) string {
	as, ok := flow["as"].(string)
	if !ok {
		return ""
	}
	delete(flow, "as")
	if as == "" || as == defaultSession {
		return fmt.Sprintf("flow-level as %q dropped: steps without as already run as main", as)
	}
	for _, sv := range steps {
		if st, ok := sv.(map[string]any); ok {
			if _, has := st["as"]; !has {
				st["as"] = as
			}
		}
	}
	return fmt.Sprintf("flow-level as %q moved onto each step (as is a step qualifier)", as)
}

// expandSessionMarkers handles a step that is ONLY {as: "bob"}: a "switch to bob"
// marker, the way other tools switch browser context. Every following step without
// an as of its own runs as that session until the next marker ({as: "main"} switches
// back), and the markers themselves are dropped. Returns the rewritten steps and a
// note, or the steps unchanged and "".
func expandSessionMarkers(steps []any) ([]any, string) {
	marker := func(sv any) (string, bool) {
		st, ok := sv.(map[string]any)
		if !ok || len(st) != 1 {
			return "", false
		}
		as, ok := st["as"].(string)
		return as, ok
	}
	found := false
	for _, sv := range steps {
		if _, ok := marker(sv); ok {
			found = true
			break
		}
	}
	if !found {
		return steps, ""
	}
	current := ""
	out := make([]any, 0, len(steps))
	for _, sv := range steps {
		if as, ok := marker(sv); ok {
			current = as
			if current == defaultSession {
				current = ""
			}
			continue
		}
		if st, ok := sv.(map[string]any); ok && current != "" {
			if _, has := st["as"]; !has {
				st["as"] = current
			}
		}
		out = append(out, sv)
	}
	return out, "a step holding only as was read as 'the following steps run as that session' and dropped; put as on each step that user takes"
}

// dropOpeningOnlySession handles as on a flow's opening goto and nowhere else
// ({goto: "/", as: "mark"} then steps without as). The author meant "this flow's
// user", but it opens a second session while every later step runs as main on a
// blank page. main is already a fresh, logged-out visitor, so dropping the as gives
// the journey the author wrote. Returns the note, or "" when nothing changed.
func dropOpeningOnlySession(steps []any) string {
	if len(steps) < 2 {
		return ""
	}
	first, ok := steps[0].(map[string]any)
	if !ok {
		return ""
	}
	as, _ := first["as"].(string)
	if _, isGoto := first["goto"]; !isGoto || as == "" || as == defaultSession {
		return ""
	}
	for _, sv := range steps[1:] {
		if st, ok := sv.(map[string]any); ok {
			if other, _ := st["as"].(string); other != "" {
				return "" // the flow uses sessions on purpose
			}
		}
	}
	delete(first, "as")
	return fmt.Sprintf("step 1: as %q dropped - it was only on the opening goto, so the rest of the flow would have run as main on a blank page; main is already a fresh visitor", as)
}

// selectorStepKeys are the step keys whose value is an element selector.
var selectorStepKeys = []string{"click", "dblclick", "hover", "drag", "scroll", "fill", "type", "select", "check", "uncheck",
	"upload", "download", "waitFor", "expectVisible", "expectHidden", "expectCount", "snapshot", "expectChanged", "in", "to", "from"}

// unquoteTextSelector turns text="Save" or text='Save' into text=Save.
func unquoteTextSelector(sel string) (string, bool) {
	v, ok := strings.CutPrefix(sel, "text=")
	if !ok || len(v) < 2 {
		return sel, false
	}
	if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
		return "text=" + v[1:len(v)-1], true
	}
	return sel, false
}

// stepSession returns the session a raw step runs in ("" for main).
func stepSession(st map[string]any) string {
	as, _ := st["as"].(string)
	if as == defaultSession {
		return ""
	}
	return as
}

// rawVerb returns the verb key of a raw step, or "".
func rawVerb(st map[string]any) string {
	for _, v := range verbNames() {
		if _, ok := st[v]; ok {
			return v
		}
	}
	return ""
}

// inheritOpeningSession handles flows where "main" never opens a page but steps
// without as follow a goto made as another user ({goto: "/", as: "amy"}, then
// {fill: ...}): the author wrote those steps for the user who just opened the page,
// since main would act on a blank tab. Each such step takes the session of the
// nearest goto before it. Returns the note, or "" when nothing changed.
func inheritOpeningSession(steps []any) string {
	for _, sv := range steps {
		if st, ok := sv.(map[string]any); ok {
			if _, isGoto := st["goto"]; isGoto && stepSession(st) == "" {
				return "" // main opens a page itself: steps without as are its own
			}
		}
	}
	current, n := "", 0
	for _, sv := range steps {
		st, ok := sv.(map[string]any)
		if !ok {
			continue
		}
		if _, isGoto := st["goto"]; isGoto {
			current = stepSession(st)
			continue
		}
		if _, has := st["as"]; !has && current != "" {
			st["as"] = current
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("main never opens a page here, so %d step(s) without as were run as the user whose goto came before them; put as on every step a user takes", n)
}

// flowActionVerbs are the verbs that act on the page (and so can change what an
// element looks like), as opposed to waits and assertions.
var flowActionVerbs = map[string]bool{
	"click": true, "dblclick": true, "hover": true, "drag": true, "scroll": true, "fill": true, "type": true,
	"select": true, "check": true, "uncheck": true, "press": true, "upload": true, "download": true,
}

// insertMissingSnapshots handles expectChanged without an earlier snapshot of the
// same element in the same session: "the last input changed it" is the only
// reading, so a snapshot goes right before the nearest action before it (after that
// session's latest goto, so the page it snapshots is the one the action changes).
// An expectChanged with no such action is left for validation to reject.
func insertMissingSnapshots(steps []any) ([]any, string) {
	type key struct{ session, sel string }
	snapped := map[key]bool{}
	insertAt := map[int][]map[string]any{}
	n := 0
	for i, sv := range steps {
		st, ok := sv.(map[string]any)
		if !ok {
			continue
		}
		verb, sess := rawVerb(st), stepSession(st)
		sel, _ := st[verb].(string)
		switch verb {
		case "snapshot":
			snapped[key{sess, sel}] = true
		case "expectChanged":
			if snapped[key{sess, sel}] {
				continue
			}
			at := -1
			for j := i - 1; j >= 0; j-- {
				prev, ok := steps[j].(map[string]any)
				if !ok || stepSession(prev) != sess {
					continue
				}
				pv := rawVerb(prev)
				if pv == "goto" || pv == "reload" {
					break
				}
				if flowActionVerbs[pv] {
					at = j
					break
				}
			}
			if at < 0 {
				continue
			}
			snap := map[string]any{"snapshot": sel}
			if as, ok := st["as"]; ok {
				snap["as"] = as
			}
			insertAt[at] = append(insertAt[at], snap)
			snapped[key{sess, sel}] = true
			n++
		}
	}
	if n == 0 {
		return steps, ""
	}
	out := make([]any, 0, len(steps)+n)
	for i, sv := range steps {
		for _, snap := range insertAt[i] {
			out = append(out, snap)
		}
		out = append(out, sv)
	}
	return out, fmt.Sprintf("%d expectChanged step(s) had no snapshot to compare against, so a snapshot of the same element was taken right before the action that precedes them", n)
}

// normalizeSessionName lowercases a session name and drops what the session pattern
// does not allow, returning "" when nothing valid is left.
func normalizeSessionName(as string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(as) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 20 {
		out = out[:20]
	}
	if !flowSessionRe.MatchString(out) {
		return ""
	}
	return out
}
