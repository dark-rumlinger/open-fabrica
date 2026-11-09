package engine

// A user-flow finding in the final sweep is shown to the owner as a flow that did
// not pass, not as a known issue: the script may be at fault, not the app.

import (
	"fmt"
	"regexp"
	"strings"

	"openfabrica/probe"
	"openfabrica/store"
)

// flowCategory is the validator category of a user-flow finding.
const flowCategory = "flow"

// flowTestsValidatorName is the registered name of the user-flow validator.
const flowTestsValidatorName = "flow_tests"

// flowIssueNameRe reads the flow's name out of a flow finding's message.
var flowIssueNameRe = regexp.MustCompile(`^user flow "([^"]+)"`)

// withoutFlowValidator returns the validators minus the user-flow one.
func withoutFlowValidator(all []Validator) []Validator {
	kept := make([]Validator, 0, len(all))
	for _, v := range all {
		if v.Name() != flowTestsValidatorName {
			kept = append(kept, v)
		}
	}
	return kept
}

// onlyFlowIssues reports whether issues is non-empty and every one is a flow finding.
func onlyFlowIssues(issues []Issue) bool {
	if len(issues) == 0 {
		return false
	}
	for _, is := range issues {
		if is.Category != flowCategory {
			return false
		}
	}
	return true
}

// unverifiedFlows names the journeys the final sweep still reports, or a generic entry
// when a finding names none.
func unverifiedFlows(latest []Issue) []string {
	var names []string
	seen := map[string]bool{}
	for _, is := range latest {
		if is.Category != flowCategory || is.Severity != store.SeverityError {
			continue
		}
		name := "the user-flow script"
		if m := flowIssueNameRe.FindStringSubmatch(is.Message); m != nil {
			name = m[1]
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// flowCheck is the user flow test result carried by the go-live card payload.
// Total is the number of flows in the saved script; a skipped replay verified none
// of them, which is not the same as all of them passing.
type flowCheck struct {
	Total    int      `json:"total"`
	Failed   []string `json:"failed,omitempty"`
	Skipped  bool     `json:"skipped,omitempty"`
	SkipKind string   `json:"skip_kind,omitempty"`
}

// buildFlowCheck summarises the final sweep for the go-live card, or returns nil when
// there is nothing to say (no script and no finding).
func buildFlowCheck(latest []Issue, script string) *flowCheck {
	fc := &flowCheck{Failed: unverifiedFlows(latest)}
	if script != "" {
		if suite, err := probe.ParseFlowSource(script); err == nil {
			fc.Total = len(suite.Flows)
		}
	}
	for _, is := range latest {
		if is.Category == flowCategory && strings.HasPrefix(is.Signature, probe.FlowSkippedSignaturePrefix) {
			fc.Skipped = true
			fc.SkipKind = strings.TrimPrefix(is.Signature, probe.FlowSkippedSignaturePrefix)
			break
		}
	}
	if fc.Total == 0 && len(fc.Failed) == 0 && !fc.Skipped {
		return nil
	}
	return fc
}

// flowCheckFor builds the go-live flow result from the final sweep. It is nil when the
// owner turned the tests off or the sweep could not reach the project, because the
// flows were not replayed and must not read as passed.
func (w *Worker) flowCheckFor(latest []Issue) *flowCheck {
	if !w.userFlowsEnabled() {
		return nil
	}
	if baseURL, _ := w.probeTarget(); baseURL == "" {
		return nil
	}
	return buildFlowCheck(latest, probe.LoadFlowSource(w.pdb.Reader()))
}

// flowUnverifiedSection is the go-live note for failed flows, empty when there are none.
func flowUnverifiedSection(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("\n**Flow tests that did not pass (%d)**\n\n- %s\n\nThe replay failed for these. The app or the flow itself may be out of date.\n",
		len(names), summarizeNames(names))
}

// flowSkippedSection is the go-live note for a replay that did not run, empty otherwise.
func flowSkippedSection(fc *flowCheck) string {
	if fc == nil || !fc.Skipped {
		return ""
	}
	return "\n**Flow tests were not replayed**\n\nThe flows stay saved and run again where a browser is available. This says nothing about the app.\n"
}

// flowUnverifiedTail is the closing line when only flow tests are outstanding.
func flowUnverifiedTail() string {
	return "Tell me to look at the failed flows and I'll fix the app or update the flow."
}
