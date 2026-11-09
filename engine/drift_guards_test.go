package engine

// Preventive drift guards, each closing a gap the existing suite did not cover:
// README env-var defaults vs config/defaults.go; {{contract:id}} tokens in any
// pack body resolving to a real contract (a typo'd token would otherwise reach
// the PLAN prompt unrendered); the programmatic e2ee-security-model contract
// reference still resolving (named only by a Go string literal, so a rename
// fails only at runtime).

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"openfabrica/config"
	"openfabrica/contracts"
	"openfabrica/pack"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", rel)
}

// TestReadmeEnvDefaultsMatchConfig asserts the README env-var table documents the
// same defaults the code actually uses. Drives expected values from the config
// constants, so a config change with a stale README row fails here.
func TestReadmeEnvDefaultsMatchConfig(t *testing.T) {
	raw, err := os.ReadFile(repoFile(t, "README.md"))
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}
	readme := string(raw)

	want := map[string]string{
		"OPENFABRICA_ADMIN_PORT":                strconv.Itoa(config.DefaultAdminPort),
		"OPENFABRICA_PUBLIC_PORT":               strconv.Itoa(config.DefaultPublicPort),
		"OPENFABRICA_RATE_LIMIT_RATE":           strconv.Itoa(config.DefaultRateLimitRate),
		"OPENFABRICA_RATE_LIMIT_BURST":          strconv.Itoa(config.DefaultRateLimitBurst),
		"OPENFABRICA_LLM_TIMEOUT":               strconv.Itoa(config.DefaultLLMTimeoutSec),
		"OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET": strconv.Itoa(config.DefaultEngineDailyTokenBudget),
	}

	for v, expected := range want {
		row := tableRowFor(readme, v)
		if row == "" {
			t.Errorf("README env-var table has no row for %s", v)
			continue
		}
		def := tableDefaultCell(row)
		if def != expected {
			t.Errorf("README documents %s default %q but config uses %q", v, def, expected)
		}
	}
}

// tableRowFor returns the README table row whose first cell names the env var, or "".
func tableRowFor(readme, varName string) string {
	for _, line := range strings.Split(readme, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.Contains(line, "`"+varName+"`") {
			return line
		}
	}
	return ""
}

// tableDefaultCell returns the trimmed second column of a `| name | default | ... |` row.
func tableDefaultCell(row string) string {
	cells := strings.Split(row, "|") // ["", " `VAR` ", " default ", " desc ", ""]
	if len(cells) < 3 {
		return ""
	}
	return strings.TrimSpace(cells[2])
}

// TestPackContractTokensResolve asserts every {{contract:id}} token in any pack
// body (stage frames, fragments, AND capability guides - the last inlined as their
// core at PLAN/BUILD via RenderGuidesCore) names a contract that actually loads. A
// typo'd token would otherwise survive unrendered into the prompt.
func TestPackContractTokensResolve(t *testing.T) {
	all, err := contracts.Load()
	if err != nil {
		t.Fatalf("contracts.Load: %v", err)
	}
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	// pack.ContractTokenIDs, not a pattern of our own: the assembler passes an
	// unknown id through to the model verbatim, so this guard is the only thing
	// standing between a typo and a literal {{contract:...}} in the prompt. A guard
	// that recognizes fewer token shapes than the assembler resolves silently stops
	// covering the difference.
	for _, p := range ps {
		for _, id := range pack.ContractTokenIDs(p.Body) {
			if _, ok := all[id]; !ok {
				t.Errorf("pack %s/%s references {{contract:%s}}, which is not a real contract", p.Role, p.Slug, id)
			}
		}
	}
}

// TestPlanSecurityContractResolves guards the one contract reference made by a Go
// string literal (planSecurityContract -> contracts.Render("e2ee-security-model")):
// a renamed contract id compiles fine and would silently inject nothing at PLAN
// time. Assert an encryption brief still yields a non-empty model, and a plain
// brief yields none.
func TestPlanSecurityContractResolves(t *testing.T) {
	if got := planSecurityContract("an end-to-end encrypted private chat app"); strings.TrimSpace(got) == "" {
		t.Error("planSecurityContract returned empty for an encryption brief - the e2ee-security-model contract reference may have drifted")
	}
	if got := planSecurityContract("a public blog with posts and comments"); got != "" {
		t.Errorf("planSecurityContract must be empty for a non-encryption brief, got %q", got)
	}
}

// TestBriefImpliesEncryptionAccountVeto pins the account-shape veto: a brief
// whose "password" belongs to user ACCOUNTS (register/login, admin roles,
// per-account server data) must NOT get the E2EE security model appended -
// that injection contradicts the brief's own auth requirements. Account wording
// co-occurring with "chat" is not sufficient to imply E2EE. Explicit encryption
// wording still wins.
func TestBriefImpliesEncryptionAccountVeto(t *testing.T) {
	minecraft := "A browser-based multiplayer Minecraft clone. Players register an account with username and password and log in; an admin role can kick players and reset the world; inventory is persistent per account. A chat overlay lets everyone talk."
	if briefImpliesEncryption(minecraft) {
		t.Error("an accounts-shaped brief (register/login/admin) must not imply the E2EE model, even with password+chat co-occurring")
	}
	room := "A private watch-party room you join with a room name and password, with live chat."
	if !briefImpliesEncryption(room) {
		t.Error("a password-joined private room brief must still imply the E2EE model")
	}
	explicit := "An end-to-end encrypted messenger where users register accounts."
	if !briefImpliesEncryption(explicit) {
		t.Error("explicit encryption wording must always imply the model, even alongside accounts")
	}
}

// TestBriefImpliesEncryptionContentSecrecyPhrases pins the broadened matcher: a brief
// that states E2EE intent WITHOUT the literal "encrypt" ("only participants can read",
// "disappearing messages") implies the model, while the account veto still overrides
// those phrases (E2EE replaces server-side auth).
func TestBriefImpliesEncryptionContentSecrecyPhrases(t *testing.T) {
	for _, brief := range []string{
		"A group chat where a message is readable by only participants; nobody else can read it.",
		"A disappearing message app for private one-to-one conversations.",
	} {
		if !briefImpliesEncryption(brief) {
			t.Errorf("a content-secrecy brief should imply the E2EE model: %q", brief)
		}
	}
	vetoed := "A team messenger with user accounts and login where only participants can read a thread."
	if briefImpliesEncryption(vetoed) {
		t.Error("an accounts-shaped brief must not imply E2EE even with a content-secrecy phrase")
	}
}

// docTestNameRe matches a backticked Go test name in a markdown doc. Backticks are
// the corpus convention for naming a test by identifier, and requiring them is what
// keeps prose out of the scan: the "`*_drift` tests" glob and the
// "`TestDocsReferenceRealTools` / `…RealActions`" ellipsis shorthand both stay
// unmatched, so only a token the author meant as an exact name is checked.
var docTestNameRe = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")

// declaredTestRe matches a test function declaration in a _test.go file.
var declaredTestRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

// goCommentTestNameRe matches a test name cited in a Go // comment, as submatch 1.
// Go comments do not use the markdown backtick convention, so the anchor is the bare
// identifier - which is safe because goSymbolRe below subtracts every Test-prefixed
// name that is a real declaration, and no allowlist is needed to tell a citation from
// a symbol. The leading non-word alternative stands in for a word boundary so the
// pattern carries no escape at all.
var goCommentTestNameRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(Test[A-Z][A-Za-z0-9_]*)`)

// goLineCommentRe captures the text of a // comment. String literals are deliberately
// out of scope: a Test-prefixed name inside one is data, not provenance.
var goLineCommentRe = regexp.MustCompile(`//(.*)`)

// goSymbolRe matches a Test-prefixed FUNCTION, METHOD, or TYPE declaration anywhere in
// the tree. A comment naming one of those (ProvidersHandler.TestConfig,
// BotHandler.TestTelegram) is documenting that symbol, not citing a test, so those
// names are subtracted from the scan.
var goSymbolRe = regexp.MustCompile(`(?m)^\s*(?:func\s+(?:\([^)]*\)\s*)?|type\s+)(Test[A-Za-z0-9_]*)`)

// TestDocsCiteRealTestNames asserts every test a doc names by identifier still
// exists. The authoring docs cite guard tests densely - naming the test IS how a
// reader finds the enforcement behind a rule - so a renamed test leaves the
// citation pointing at nothing while still reading as working provenance, which
// is worse than no citation. The same preventive shape as
// TestReadmeEnvDefaultsMatchConfig above, run over the whole corpus rather than a
// listed set so a new doc is covered without being registered anywhere.
//
// Go COMMENTS are scanned on the same terms, because guard citations live there too:
// e.g. a helper documented as kept in step by a named test that does not exist.
// Scanning only .md would leave the most load-bearing citations unchecked: a comment
// claiming a guard stands behind an invariant, or a doc comment headed by a stale
// function name, one file type away from a guard built for exactly that.
func TestDocsCiteRealTestNames(t *testing.T) {
	root := repoFile(t, ".")

	declared := map[string]bool{}  // Test funcs in _test.go: the citable set
	goSymbols := map[string]bool{} // Test-prefixed funcs/methods/types: named, not cited
	docs := map[string][]byte{}
	var docPaths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // an unreadable subtree is not this guard's business
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "data", "bin", "release", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		isGo := strings.HasSuffix(path, ".go")
		if !isGo && !strings.HasSuffix(path, ".md") {
			return nil // check the suffix before reading: the tree carries large binaries
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if isGo {
			if strings.HasSuffix(path, "_test.go") {
				for _, m := range declaredTestRe.FindAllSubmatch(src, -1) {
					declared[string(m[1])] = true
				}
			}
			for _, m := range goSymbolRe.FindAllSubmatch(src, -1) {
				goSymbols[string(m[1])] = true
			}
		}
		docs[path] = src
		docPaths = append(docPaths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}
	// A walk that saw nothing would pass vacuously, which is the one way this
	// guard could go quietly blind.
	if len(declared) == 0 || len(docPaths) == 0 {
		t.Fatalf("walk found %d test declarations and %d files - it is not seeing the repo", len(declared), len(docPaths))
	}

	sort.Strings(docPaths)
	for _, path := range docPaths {
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		reported := map[string]bool{}
		report := func(name string) {
			if declared[name] || reported[name] {
				return
			}
			reported[name] = true
			t.Errorf("%s cites %s, which no _test.go declares - a renamed or deleted test",
				filepath.ToSlash(rel), name)
		}
		if strings.HasSuffix(path, ".go") {
			for _, c := range goLineCommentRe.FindAllSubmatch(docs[path], -1) {
				for _, m := range goCommentTestNameRe.FindAllSubmatch(c[1], -1) {
					name := string(m[1])
					// A comment naming a real Test-prefixed symbol is documenting that
					// symbol, not citing a test.
					if goSymbols[name] {
						continue
					}
					report(name)
				}
			}
			continue
		}
		for _, m := range docTestNameRe.FindAllSubmatch(docs[path], -1) {
			report(string(m[1]))
		}
	}
}
