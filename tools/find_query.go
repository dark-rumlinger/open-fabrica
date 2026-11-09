package tools

import "strings"

// FindQueryLog remembers the find_tool searches that matched nothing, so a paraphrase
// of one can be refused instead of run again. find_tool's own description promises
// "rewording the same search is refused", so every loop that services find_tool (the
// engine's sessions and the owner chat) keeps one of these for its conversation.
type FindQueryLog struct {
	queries []findQuery
}

// findQuery is one find_tool search, kept as the text the model wrote plus its
// normalised terms so a paraphrase compares against it.
type findQuery struct {
	raw   string
	terms map[string]bool
}

// findQueryStopwords are terms too generic to carry intent, dropped so two
// searches are compared on their subject rather than their filler.
var findQueryStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"tool": true, "tools": true, "manage": true, "any": true, "how": true,
}

// findQueryTerms normalises a search into a comparable term set: lowercase,
// non-alphanumeric split, short and generic terms dropped, trailing plural "s"
// trimmed so "image" and "images" compare equal.
func findQueryTerms(q string) map[string]bool {
	out := map[string]bool{}
	for _, term := range findQueryTermList(q) {
		out[term] = true
	}
	return out
}

// findQueryTermList is findQueryTerms in query order with duplicates removed, so the
// catalogue ranker and the repeat guard read a query the same way.
func findQueryTermList(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, term := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(term) > 3 && strings.HasSuffix(term, "s") {
			term = strings.TrimSuffix(term, "s")
		}
		if len(term) < 3 || findQueryStopwords[term] || seen[term] {
			continue
		}
		seen[term] = true
		out = append(out, term)
	}
	return out
}

// findQueryRepeatRatio is the term overlap at which two searches are treated as
// the same question. Half the smaller term set, with a floor of two shared terms
// for multi-term sets: one shared word ("web search" vs "keyword search") is a
// shared subject, not a restated question, and refusing it would deny a tool
// that exists. Single-term sets still match on their one term.
const findQueryRepeatRatio = 0.5

// Repeat returns the earlier failed search this one restates, or "" when it is new.
func (l *FindQueryLog) Repeat(q string) string {
	terms := findQueryTerms(q)
	if len(terms) == 0 {
		return ""
	}
	for _, prior := range l.queries {
		if len(prior.terms) == 0 {
			continue
		}
		shared := 0
		for t := range terms {
			if prior.terms[t] {
				shared++
			}
		}
		// Cutting a long failed search down to ONE of its own words is a corrected,
		// simpler query, not a restatement: the multi-word search may have missed only
		// because of its filler. A two-word cut of the same words is still a reword.
		if len(prior.terms) >= 3 && len(terms) == 1 && subset(terms, prior.terms) {
			continue
		}
		smaller := len(terms)
		if len(prior.terms) < smaller {
			smaller = len(prior.terms)
		}
		if smaller >= 2 && shared < 2 {
			continue
		}
		if float64(shared) >= findQueryRepeatRatio*float64(smaller) {
			return prior.raw
		}
	}
	return ""
}

// subset reports whether every term of a is in b.
func subset(a, b map[string]bool) bool {
	for t := range a {
		if !b[t] {
			return false
		}
	}
	return true
}

// Record remembers a search that matched nothing, so a later paraphrase is refused.
func (l *FindQueryLog) Record(q string) {
	l.queries = append(l.queries, findQuery{raw: strings.TrimSpace(q), terms: findQueryTerms(q)})
}

// RepeatSearchResult is the find_tool answer for a search that restates prior, one
// that already matched nothing.
func RepeatSearchResult(prior string) FindToolResult {
	return FindToolResult{Success: true, Note: "You already searched for this (\"" + prior +
		"\") and nothing matched. Rewording it will not change the answer. Take the fallback named in that result, or build the feature without the tool."}
}
