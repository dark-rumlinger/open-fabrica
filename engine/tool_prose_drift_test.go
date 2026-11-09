package engine

// Negative anti-pattern guard for LLM-facing prose the doc<->registry suite
// does not otherwise cover: ToolDef free-text (OneLiner, WhenToUse) and every
// embedded pack/contract body describing the REST LIST response with the WRONG
// envelope key. The canonical LIST envelope is {rows, count, limit, offset} -
// the array is under `rows`, with explicitly no `.data` field; a stale copy
// naming the list array `data` makes the builder emit code that throws `x.map
// is not a function` at runtime. TestGovernedDocsMatchContracts can't catch
// this: its POSITIVE probes are satisfied by a wrong-envelope line, so only a
// negative scan for the anti-pattern closes the gap.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"openfabrica/tools"
)

// wrongListEnvelopePatterns match a LIST response envelope described with the
// non-existent `data` key instead of the canonical `rows`. Kept deliberately
// specific so the legitimate contract prose - "there is no `.data` field", the
// "double-`.data` trap" - never trips them.
var wrongListEnvelopePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)envelope\s*\{\s*data\b`),
	regexp.MustCompile(`(?i)\{\s*data\s*,\s*count\s*,\s*limit\s*,\s*offset`),
}

// TestNoWrongListEnvelopeInToolProse scans every registered tool's LLM-facing
// prose (OneLiner + WhenToUse) for the wrong-envelope anti-pattern. This is the
// field manage_api's WhenToUse drifted on while every other copy stayed correct.
func TestNoWrongListEnvelopeInToolProse(t *testing.T) {
	list := tools.DefaultRegistry().List()
	if len(list) < 20 {
		t.Fatalf("registry looks unpopulated (%d tools) - are the tool subpackages imported?", len(list))
	}
	for _, td := range list {
		prose := td.OneLiner + "\n" + td.WhenToUse
		for _, re := range wrongListEnvelopePatterns {
			if hit := re.FindString(prose); hit != "" {
				t.Errorf("tool %q prose describes the LIST envelope as %q; the canonical key is `rows` "+
					"({rows, count, limit, offset}), not `data` - see knowledge/contracts/api-response.contract.md",
					td.Name, hit)
			}
		}
	}
}

// TestNoWrongListEnvelopeInDocs scans every embedded pack (stage frames, guides,
// fragments) and contract body - the same corpus docs_consistency_test.go
// checks - for the wrong-envelope anti-pattern, so the canonical `rows` envelope
// can never be restated as `{data,...}` anywhere the build loop feeds the model.
func TestNoWrongListEnvelopeInDocs(t *testing.T) {
	for label, body := range docSources(t) {
		for _, re := range wrongListEnvelopePatterns {
			if hit := re.FindString(body); hit != "" {
				t.Errorf("%s describes the LIST envelope as %q; the canonical key is `rows`, not `data`",
					label, hit)
			}
		}
	}
}

// wrongDataLayerEnvelopeRe matches a LIST response for a `/api/<path>` endpoint
// described with the non-existent `data` key instead of the canonical `rows`
// (e.g. `GET /api/<path> -> {data:[...], count, limit, offset}`). The runtime
// "Data Layer" summary is assembled from Go string literals, not embedded
// prose, so TestNoWrongListEnvelopeInDocs never sees it. `->` is not Go syntax,
// so this only ever matches doc-like strings, never real code.
var wrongDataLayerEnvelopeRe = regexp.MustCompile(`(?i)/api/[^\n]*->\s*\{\s*data\b`)

// TestNoWrongListEnvelopeInGoSource scans production (non-test) Go source for a
// LIST response envelope described with `{data...}` instead of `{rows...}`,
// closing the gap the doc/prose scanners leave for runtime-built prompt strings.
func TestNoWrongListEnvelopeInGoSource(t *testing.T) {
	root := repoRootDir(t)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, don't fail the walk
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "assets", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if hit := wrongDataLayerEnvelopeRe.Find(b); hit != nil {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s describes a LIST response as %q; the canonical envelope key is `rows` "+
				"({rows, count, limit, offset}), not `data` - see knowledge/contracts/api-response.contract.md",
				filepath.ToSlash(rel), string(hit))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
}

// blobChunkAsEnvelopeRe matches an encrypted BLOB chunk described as the chat
// envelope. The e2ee-wire contract fixes the two shapes apart on purpose: a chat
// frame is `{iv, ct}`, while encryptBlob returns `{meta, chunks:[base64]}` where
// each chunk is ONE opaque base64 string with its IV prepended. A doc that tells
// the builder to send blob chunks as top-level `{iv,ct}` frames produces exactly
// what that split exists to prevent - the relay reads them as chat envelopes and
// persists them.
//
// Only the wrong direction is matched (envelope, then "chunk" close after), and a
// hit that is being NEGATED ("not an {iv,ct} object", "never an {iv,ct} frame") is
// dropped, so the contract's own statement of the distinction stays clean while a
// doc that asserts it slips nothing past.
var blobChunkAsEnvelopeRe = regexp.MustCompile(`(?i)\{\s*iv\s*,\s*ct\s*\}[^\n.]{0,40}chunk`)

// blobChunkNegations are the words that, right before an envelope mention, mean the
// text is drawing the distinction rather than teaching the wrong shape.
var blobChunkNegations = []string{"not ", "never ", "rather than ", "instead of "}

// findBlobChunkAsEnvelope returns the offending phrase, or "" when every hit in
// body is a negation.
func findBlobChunkAsEnvelope(body string) string {
	for _, loc := range blobChunkAsEnvelopeRe.FindAllStringIndex(body, -1) {
		lead := body[max(0, loc[0]-24):loc[0]]
		negated := false
		for _, n := range blobChunkNegations {
			if strings.Contains(strings.ToLower(lead), n) {
				negated = true
				break
			}
		}
		if !negated {
			return body[loc[0]:loc[1]]
		}
	}
	return ""
}

// TestNoBlobChunkTaughtAsChatEnvelope scans every surface that reaches the model
// with this claim: the embedded packs and contracts, each tool's own prose, and the
// production Go that builds tool RESULTS (where two copies of the wrong shape lived
// while the governing contract, the helper, and the relay's own rejection message
// all said the right thing).
func TestNoBlobChunkTaughtAsChatEnvelope(t *testing.T) {
	report := func(label, body string) {
		if hit := findBlobChunkAsEnvelope(body); hit != "" {
			t.Errorf("%s describes a blob chunk as the chat envelope (%q). encryptBlob returns "+
				"{meta, chunks:[base64]} - each chunk is ONE opaque base64 string, never an {iv,ct} "+
				"object - see knowledge/contracts/e2ee-wire.contract.md", label, hit)
		}
	}
	for label, body := range docSources(t) {
		report(label, body)
	}
	for label, body := range toolProseSources(t) {
		report(label, body)
	}
	for label, body := range productionGoSources(t) {
		report(label, body)
	}
}

// productionGoSources returns every non-test Go file body in the repo, keyed by
// repo-relative path. Prompt text assembled from Go string literals (a tool result,
// a rejection message) reaches the model exactly like an embedded pack does, but no
// pack scanner sees it.
func productionGoSources(t *testing.T) map[string]string {
	t.Helper()
	root := repoRootDir(t)
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "assets", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// capabilityAsActionRe matches a manage_* tool name immediately followed by a
// plan-capability name as if the capability were one of that tool's action
// verbs. The capability vocabulary is deliberate PLAN shorthand ("a user table
// + `create_auth`"), but juxtaposed with a tool name it reads as a call form -
// "call manage_auth <capability>" - and a model following it literally passes
// the capability as `action` and gets an invalid-action rejection (the real
// verb is `create`). TestDocsReferenceRealActions only parses `action=verb`
// syntax, so this shape slips through it. Optional backticks cover the
// markdown-formatted variant.
var capabilityAsActionRe = regexp.MustCompile(
	"manage_[a-z0-9_]+`?\\s+`?" + `create_(?:api|auth|oauth|websocket|stream|llm)\b`)

// TestNoCapabilityNameUsedAsAction scans the corpora a build model actually
// reads - registered tool prose, embedded packs/contracts, and Go source
// (runtime error strings and hints come back as tool results, exactly where
// this shape drifted in) - for a capability name used as an action verb.
func TestNoCapabilityNameUsedAsAction(t *testing.T) {
	list := tools.DefaultRegistry().List()
	if len(list) < 20 {
		t.Fatalf("registry looks unpopulated (%d tools) - are the tool subpackages imported?", len(list))
	}
	for _, td := range list {
		prose := td.OneLiner + "\n" + td.WhenToUse
		if hit := capabilityAsActionRe.FindString(prose); hit != "" {
			t.Errorf("tool %q prose says %q - a plan capability name used as an action verb; "+
				"name the real action (e.g. action=create) instead", td.Name, hit)
		}
	}
	for label, body := range docSources(t) {
		if hit := capabilityAsActionRe.FindString(body); hit != "" {
			t.Errorf("%s says %q - a plan capability name used as an action verb; "+
				"name the real action (e.g. action=create) instead", label, hit)
		}
	}
	root := repoRootDir(t)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, don't fail the walk
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "assets", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if hit := capabilityAsActionRe.Find(b); hit != nil {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s says %q - a plan capability name used as an action verb; "+
				"name the real action (e.g. action=create) instead", filepath.ToSlash(rel), string(hit))
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
}
