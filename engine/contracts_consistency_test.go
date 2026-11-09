package engine

import (
	"fmt"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/pack"
)

// docTexts returns every embedded pack body keyed by slug - the corpus the
// contract drift check scans.
func docTexts(t *testing.T) map[string]string {
	t.Helper()
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	out := make(map[string]string, len(ps))
	for _, p := range ps {
		out[p.Slug] = p.Body
	}
	return out
}

// TestGovernedDocsMatchContracts is the drift guard that closes Pain A: every doc
// a contract governs must EITHER transclude the fact via `{{contract:id}}`
// (drift-proof by construction) OR carry the contract's canonical probe tokens
// inline. A reworded or dropped inline copy of a fact - the auth_token key, the
// realtime presence-seeding rule, the e2ee wire envelope - fails here, so the
// single source of truth and its restatements can no longer silently diverge.
func TestGovernedDocsMatchContracts(t *testing.T) {
	all, err := contracts.Load()
	if err != nil {
		t.Fatalf("contracts.Load: %v", err)
	}
	docs := docTexts(t)

	for _, c := range all {
		token := fmt.Sprintf("{{contract:%s}}", c.ID)
		for _, slug := range c.Governs {
			text, ok := docs[slug]
			if !ok {
				t.Errorf("contract %q governs unknown pack slug %q", c.ID, slug)
				continue
			}
			if strings.Contains(text, token) {
				continue // transcluded - the fact is single-sourced, no drift possible
			}
			if !c.Matches(text) {
				t.Errorf("pack %q neither transcludes {{contract:%s}} nor carries its probe tokens %v - "+
					"the fact has drifted from knowledge/contracts/%s.contract.md",
					slug, c.ID, c.Probes, c.ID)
			}
		}
	}
}
