package probe

// Advisory save-time lints for flows that prove less than their name claims. They only warn.

import (
	"fmt"
	"regexp"
	"strings"
)

// flowMultiUserNameRe matches flow names that promise more than one person is involved.
var flowMultiUserNameRe = regexp.MustCompile(`(?i)\b(two|2|both|multiple|several|other)\s+(players?|users?|people|person|browsers?|tabs?|clients?|visitors?)\b|each other|multi-?player|collaborat|real-?time|presence`)

// flowPresenceNameRe matches names that promise VISIBLE peer presence (peers seen as
// entities in a shared space), the case where a departed peer must be seen to despawn.
// Narrower than flowMultiUserNameRe on purpose: a plain two-user chat exchanges messages
// and need not test a leave, so it is not matched here.
var flowPresenceNameRe = regexp.MustCompile(`(?i)each other|presence|multi-?player|\broster\b|\blobby\b|\bworld\b|\bonline\b|\b(players?|peers?|avatars?|tanks?|racers?|ships?)\b`)

// flowInteractionNameRe matches flow names that promise an interaction.
var flowInteractionNameRe = regexp.MustCompile(`(?i)\b(add|create|send|submit|upload|chat|log ?in|sign ?(up|in)|delete|remove|edit|save|join|drag|draw|buy|pay|checkout|search|filter|toggle|post|comment|reply|book|order)\b`)

// flowAlwaysTrueRe matches expectJs expressions that hold for any state: a count or
// number compared against zero with >=, or a literal truthy value.
var flowAlwaysTrueRe = regexp.MustCompile(`(?i)(>=\s*0(?:[^\d.]|$))|^\s*(true|1|!!document\.body|document\.body|!!window|!!document)\s*;?\s*$`)

// flowStorageRe matches assertions on how the app stores data internally.
var flowStorageRe = regexp.MustCompile(`\b(localStorage|sessionStorage|indexedDB)\b`)

// flowTestGlobalRe matches reads of a test-only global such as window.__score.
var flowTestGlobalRe = regexp.MustCompile(`\b(window\.)?__[A-Za-z]\w*`)

// lintPresenceDespawn is a SUITE-level check: a suite with a multiplayer/presence flow
// that acts as a second user must also prove the despawn side somewhere, that a peer who
// LEAVES is seen to disappear. A reducer or onPeers handler that never removes a departed
// peer (the ghost-peer bug) otherwise ships green, since a join-only flow passes with the
// ghost still on screen. Checked across the whole suite, so one dedicated leave flow
// satisfies a separate join flow. Advisory only.
func lintPresenceDespawn(s *FlowSuite) []string {
	var presenceFlow string
	leaveThenGone := false
	for _, f := range s.Flows {
		multi := flowPresenceNameRe.MatchString(f.Name)
		secondUser := false
		leaveAt := -1
		for i, st := range f.Steps {
			if st.As != "" {
				secondUser = true
			}
			switch v, _ := st.verb(); v {
			case "leave":
				if leaveAt < 0 {
					leaveAt = i
				}
			case "expectHidden", "expectNoText", "expectText", "expectJs", "expectCount":
				if leaveAt >= 0 {
					leaveThenGone = true // a gone-assertion after a leave, in this flow
				}
			}
		}
		if multi && secondUser && presenceFlow == "" {
			presenceFlow = f.Name
		}
	}
	if presenceFlow == "" || leaveThenGone {
		return nil
	}
	return []string{fmt.Sprintf("flow %q is a presence journey that acts as a second user, but no flow proves the despawn side: "+
		"have a peer leave (`{leave:true, as:...}`) and then assert the others see them gone (the roster count drops, a \"left\" note appears). "+
		"Without it a peer that never despawns (the ghost-peer bug) ships green.", presenceFlow)}
}

// lintProof returns the warnings for one flow.
func lintProof(f Flow) []string {
	var out []string

	usesSecondUser, hasAction := false, false
	for _, st := range f.Steps {
		if st.As != "" {
			usesSecondUser = true
		}
		switch v, _ := st.verb(); v {
		case "click", "dblclick", "fill", "type", "press", "drag", "upload", "download", "select",
			"check", "uncheck", "hover", "request", "webhook", "runSchedule":
			hasAction = true
		}
	}

	if flowMultiUserNameRe.MatchString(f.Name) && !usesSecondUser {
		out = append(out, fmt.Sprintf("flow %q is named like a journey between several people but never acts as a second user: "+
			"add steps with as:\"bob\" (goto, then act as the first user and expect the result as bob), or rename the flow to what it really checks. "+
			"As written it passes for a single-user app.", f.Name))
	}
	if flowInteractionNameRe.MatchString(f.Name) && !hasAction {
		out = append(out, fmt.Sprintf("flow %q is named like an interaction but has no step that acts (click, fill, type, press, upload, request): "+
			"it only loads and looks, so it passes when the interaction is broken. Do the action the name promises, then assert its result.", f.Name))
	}

	for i, st := range f.Steps {
		if v, arg := st.verb(); v == "expectJs" {
			if flowAlwaysTrueRe.MatchString(arg) {
				out = append(out, fmt.Sprintf("flow %q step %d %s holds for any state (a comparison with >= 0 or a literal true), so it proves nothing. "+
					"Assert the value a person would notice: a count that went up, text that changed, an element that appeared.", f.Name, i+1, st))
			}
			if flowTestGlobalRe.MatchString(arg) {
				out = append(out, fmt.Sprintf("flow %q step %d %s reads a test-only global (%s). Do not add hooks to the app for a flow: "+
					"assert on what a person sees (text, visibility, counts, the URL) instead.",
					f.Name, i+1, st, strings.TrimSpace(flowTestGlobalRe.FindString(arg))))
			}
			if flowStorageRe.MatchString(arg) {
				out = append(out, fmt.Sprintf("flow %q step %d %s asserts on browser storage, an internal detail that breaks when a key is renamed and says nothing about what "+
					"the person sees. Reload the page and assert the visible result instead (expectText, expectCount).", f.Name, i+1, st))
			}
		}
	}
	return out
}
