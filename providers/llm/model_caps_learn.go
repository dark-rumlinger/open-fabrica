// Quirk learning: one rule shape and one retry loop, shared by every transport.
//
// Each transport registers the 400 signatures it has learned to recognise. When
// a request fails, the matching rule demotes its capability in the store and the
// caller re-renders the request without the offending field. Rules live in the
// transport package that earned them; only the machinery is shared.
//
// The loop needs no attempt cap. Each rule demotes at most once per
// (provider, model), because Learn reports false the second time, so the bound
// is the number of registered rules.
package llm

import "sync"

// QuirkRule is one endpoint rejection this transport knows how to recover from.
type QuirkRule struct {
	// Cap is demoted to No when the rule fires. Must be in KnownModelCaps.
	Cap string

	// Sent reports whether this request could have carried the field, so an
	// unrelated 400 is never attributed to something we did not send.
	Sent func(CompletionRequest) bool

	// Match recognises the rejection. Typed errors only, so a transport failure
	// or a 500 whose text happens to echo the request can never latch a field off.
	Match func(*Error) bool
}

var (
	quirkMu    sync.RWMutex
	quirkRules = make(map[string][]QuirkRule)
)

// RegisterQuirkRule installs a rule for a transport. Called from each backend's
// init(), so rule order is that package's declaration order.
func RegisterQuirkRule(transport string, r QuirkRule) {
	quirkMu.Lock()
	defer quirkMu.Unlock()
	quirkRules[transport] = append(quirkRules[transport], r)
}

// QuirkRuleCount returns how many rules a transport registered. Bounds the retry
// loop and lets the conformance suite assert a call ceiling without a magic number.
func QuirkRuleCount(transport string) int {
	quirkMu.RLock()
	defer quirkMu.RUnlock()
	return len(quirkRules[transport])
}

// ValidateModelCaps reports rules naming a capability outside KnownModelCaps.
// Drained by main() at startup next to RegistrationErrors, so a typo fails boot
// rather than silently never firing.
func ValidateModelCaps() []error {
	quirkMu.RLock()
	defer quirkMu.RUnlock()
	var errs []error
	for transport, rules := range quirkRules {
		for _, r := range rules {
			if !KnownModelCaps[r.Cap] {
				errs = append(errs, &Error{
					Code:    CodeBadRequest,
					Message: "quirk rule for transport " + transport + " names unknown capability " + r.Cap,
				})
			}
		}
	}
	return errs
}

// LearnFromError applies the transport's rules to a failed request and reports
// whether anything new was learned, meaning a retry is worthwhile. Stops at the
// first rule that demotes something: one 400 names one field.
func LearnFromError(transport string, k ModelKey, req CompletionRequest, err error) bool {
	lerr, ok := AsLLMError(err)
	if !ok {
		return false
	}
	quirkMu.RLock()
	rules := quirkRules[transport]
	quirkMu.RUnlock()

	for _, r := range rules {
		if r.Sent != nil && !r.Sent(req) {
			continue
		}
		if !r.Match(lerr) {
			continue
		}
		// Already demoted means this rule cannot explain the failure a second
		// time, so keep looking rather than reporting false progress.
		if Caps.Learn(k, r.Cap, No) {
			return true
		}
	}
	return false
}

// RetryOnQuirk runs attempt, and re-runs it once per capability the failure
// teaches. attempt must re-render the request each time so one code path decides
// what a degraded request looks like.
func RetryOnQuirk[T any](transport string, k ModelKey, req CompletionRequest, attempt func() (T, error)) (T, error) {
	out, err := attempt()
	for i := 0; err != nil && i < QuirkRuleCount(transport); i++ {
		if !LearnFromError(transport, k, req, err) {
			break
		}
		out, err = attempt()
	}
	return out, err
}

// RetryOnQuirkErr is RetryOnQuirk for attempts that return only an error.
func RetryOnQuirkErr(transport string, k ModelKey, req CompletionRequest, attempt func() error) error {
	_, err := RetryOnQuirk(transport, k, req, func() (struct{}, error) {
		return struct{}{}, attempt()
	})
	return err
}
