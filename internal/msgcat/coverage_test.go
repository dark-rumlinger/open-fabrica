package msgcat

// Coverage: every message the Go side writes for a person must exist in every
// catalog. The messages are found by reading the source, so a new one fails this
// test until it is translated, and a catalog entry nothing uses fails it too.
//
// A message reaches a person through a small set of calls (the sinks below). Their
// argument is read as one of:
//
//   - a string literal: the message itself
//   - fmt.Sprintf(literal, ...): the literal is a template
//   - "text" + value + "text": a template with %s where the value is
//   - pickPhrase(a, b, ...): each alternative is a message
//
// Anything else is dynamic: text that came from somewhere else (an error from a
// library, a model, the database). Dynamic text is left in its own language, and
// the test makes each such site say so, in dynamicOK, rather than leaving it
// unnoticed.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"bytes"

	"openfabrica/internal/replylang"
)

// sink is a call whose string argument is shown to a person.
type sink struct {
	name string   // callee name (last identifier)
	pkg  string   // required package qualifier, or "" for any
	dirs []string // directories (repo-relative, slash form) it is read in; empty means everywhere
	args []int    // argument positions that carry the message; empty means all
}

var adminDirs = []string{"server/admin", "chat", "security", "server/middleware"}

var sinks = []sink{
	// HTTP errors, translated at the response writer by msgcat.LocalizeFor.
	{name: "writeError", dirs: adminDirs, args: []int{2}},
	{name: "WriteError", pkg: "httpx", dirs: adminDirs, args: []int{2}},
	{name: "writeChatError", dirs: adminDirs, args: []int{2}},
	{name: "serverError", dirs: adminDirs, args: []int{1}},
	{name: "requireIntParam", dirs: adminDirs, args: []int{3}},
	// The engine speaking in chat, translated when it is published.
	{name: "publishMessage", dirs: []string{"engine"}, args: []int{0}},
	{name: "publishError", dirs: []string{"engine"}, args: []int{0}},
	{name: "pickPhrase", dirs: []string{"engine"}},
	{name: "tr", dirs: []string{"engine"}, args: []int{0}},
	{name: "progress", dirs: []string{"engine"}, args: []int{0}},
	{name: "pauseForFailure", dirs: []string{"engine"}, args: []int{2}},
	// The Telegram bot speaking to a person, worded for each recipient.
	{name: "tr", dirs: []string{"bot"}, args: []int{0}},
	{name: "trh", dirs: []string{"bot"}, args: []int{0}},
	{name: "alertOthers", dirs: []string{"bot"}, args: []int{2}},
	// Explicit markers, usable anywhere.
	{name: "Mark", pkg: "msgcat", args: []int{0}},
	{name: "T", pkg: "msgcat", args: []int{1}},
	{name: "Localize", pkg: "msgcat", args: []int{1}},
}

// dynamicOK lists the sink arguments that are not literal messages, by file and
// source text, with the reason each is left alone. A new dynamic argument fails the
// test until someone decides which kind it is.
var dynamicOK = map[string]string{
	// Wrappers that forward a message their own callers pass as a literal. Those
	// callers are sinks themselves, so the message is found there.
	"server/admin/helpers.go|msg":     "writeError and requireIntParam forward their callers' messages",
	"chat/handler.go|msg":             "writeChatError forwards its callers' messages",
	"engine/publish.go|format":        "Worker.tr forwards its callers' messages",
	"engine/publish.go|text":          "the session's progress callback; the progress sink finds its literals",
	"engine/worker_drive.go|ownerMsg": "pauseForFailure forwards its callers' messages",
	"engine/worker_drive.go|msg":      "quota and budget pause lines, marked in provider.go and budget.go",

	// The bot's wrappers forward the messages their callers pass as literals.
	"bot/lang.go|format":     "voice.tr and voice.trh forward their callers' messages",
	"bot/announce.go|format": "alertOthers forwards its callers' messages",

	// Fields of a JSON body that carry text marked where it is written (firewall,
	// HTTPS and domain checks, router forwarding, startup warnings).
	"server/admin/firewall.go|st.Detail":   "firewall status lines, marked in internal/firewall",
	"server/admin/hosting.go|ds.Hint":      "HTTPS hints, marked in tlsproxy",
	"server/admin/hosting.go|st.Error":     "the router's reason, marked in internal/portmap",
	"server/admin/projects.go|w":           "domain warnings, marked in tlsproxy",
	"server/admin/projects.go|check.Error": "domain errors, marked in tlsproxy",
	"server/admin/reply_language.go|m":     "localizeAll forwards messages marked where they are written",

	// Tables whose entries are marked where they are written.
	"engine/publish.go|stageInfo(stage).StartLines":   "stage start lines, marked in each stage's Info",
	"engine/worker_drive.go|line":                     "a stage start line (marked table above) or the resume-after-answer line, both marked in publish.go",
	"server/admin/auth.go|friendlyListError(listErr)": "friendlyListError's results are marked",

	// Text another part of the system wrote, which is matched against the catalog
	// when it is one of ours (validators in this package are marked) and left in
	// its own language when it came from a library, the database or the OS.
	"server/admin/auth.go|err.Error()":              "setup and password validators are marked; the rest is technical",
	"server/admin/bot.go|err.Error()":               "messaging service errors, technical",
	"server/admin/firewall.go|err.Error()":          "operating system firewall errors",
	"server/admin/project_artifacts.go|err.Error()": "artifact store errors, technical",
	"server/admin/project_transfer.go|err.Error()":  "import manifest errors are marked",
	"server/admin/projects.go|err.Error()":          "banned address validation is marked",
	"server/admin/providers_crud.go|err.Error()":    "provider config validation, technical",
	"server/admin/providers_crud.go|cErr.Error()":   "provider config validation, technical",
	"server/admin/providers_models.go|err.Error()":  "model thinking stage validation, technical",
	"server/admin/providers_sync.go|se.msg":         "sync error messages are marked where the error is built",
	"server/admin/users.go|err.Error()":             "credential validators are marked",
	"chat/handler.go|sendErr.Error()":               "chat send errors, technical",

	// A composite of a model's sentence and one of our own lines; the line is
	// translated before it is joined (Worker.tr).
	"engine/title.go|w.titleAnnouncement(ctx, desc, title)": "the model's reaction joined to a translated naming line",
}

type location struct {
	file string
	line int
}

// collect reads the repository and returns every message and every dynamic site.
func collect(t *testing.T) (messages map[string][]location, dynamic map[string][]location) {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	messages = map[string][]location{}
	dynamic = map[string][]location{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "bin", "data", "web", "msgcat":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, pkg := calleeName(call)
			for _, s := range sinks {
				if s.name != name || (s.pkg != "" && s.pkg != pkg) || !inDirs(rel, s.dirs) {
					continue
				}
				positions := s.args
				if len(positions) == 0 {
					for i := range call.Args {
						positions = append(positions, i)
					}
				}
				for _, i := range positions {
					if i >= len(call.Args) {
						continue
					}
					msgs, dyn := messagesIn(fset, call.Args[i])
					loc := location{rel, fset.Position(call.Args[i].Pos()).Line}
					for _, m := range msgs {
						messages[m] = append(messages[m], loc)
					}
					if dyn != "" {
						dynamic[rel+"|"+dyn] = append(dynamic[rel+"|"+dyn], loc)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return messages, dynamic
}

func inDirs(rel string, dirs []string) bool {
	if len(dirs) == 0 {
		return true
	}
	for _, d := range dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

func calleeName(call *ast.CallExpr) (name, pkg string) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name, ""
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok {
			pkg = id.Name
		}
		return fn.Sel.Name, pkg
	}
	return "", ""
}

// messagesIn reads a sink argument. It returns the messages it holds, or, when the
// argument is not made of literals, the source text of the dynamic expression.
func messagesIn(fset *token.FileSet, e ast.Expr) (msgs []string, dynamic string) {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return messagesIn(fset, v.X)
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			if err == nil {
				return []string{s}, ""
			}
		}
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			break
		}
		terms := flattenAdd(v)
		var literal bool
		parts := make([]string, len(terms))
		for i, term := range terms {
			if lit, ok := term.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					parts[i] = s
					literal = true
					continue
				}
			}
			parts[i] = "\x00"
		}
		if !literal {
			break
		}
		var b strings.Builder
		hasValue := false
		for _, p := range parts {
			if p == "\x00" {
				hasValue = true
			}
		}
		for _, p := range parts {
			switch {
			case p == "\x00":
				b.WriteString("%s")
			case hasValue:
				b.WriteString(strings.ReplaceAll(p, "%", "%%"))
			default:
				b.WriteString(p)
			}
		}
		return []string{b.String()}, ""
	case *ast.CallExpr:
		name, _ := calleeName(v)
		switch name {
		case "Sprintf":
			if len(v.Args) > 0 {
				return messagesIn(fset, v.Args[0])
			}
		case "pickPhrase":
			var all []string
			for _, a := range v.Args {
				m, d := messagesIn(fset, a)
				if d != "" {
					return nil, d
				}
				all = append(all, m...)
			}
			return all, ""
		}
	}
	return nil, sourceOf(fset, e)
}

func flattenAdd(e ast.Expr) []ast.Expr {
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		return append(flattenAdd(b.X), flattenAdd(b.Y)...)
	}
	if p, ok := e.(*ast.ParenExpr); ok {
		return flattenAdd(p.X)
	}
	return []ast.Expr{e}
}

func sourceOf(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func loadCatalogFile(t *testing.T, lang string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("locales", lang+".json"))
	if err != nil {
		t.Fatalf("read %s catalog: %v", lang, err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse %s catalog: %v", lang, err)
	}
	return m
}

func nonEnglish() []string {
	var out []string
	for _, l := range replylang.Languages() {
		if l.Code != "en" {
			out = append(out, l.Code)
		}
	}
	return out
}

// Set MSGCAT_DUMP=<file> to write every message the source needs as JSON, for
// starting a translation. The file is the source of truth for what is missing.
func TestDumpMessages(t *testing.T) {
	path := os.Getenv("MSGCAT_DUMP")
	if path == "" {
		t.Skip("set MSGCAT_DUMP to write the message list")
	}
	messages, _ := collect(t)
	keys := make([]string, 0, len(messages))
	for k := range messages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = ""
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d messages written to %s", len(keys), path)
}

func TestEveryLanguageHasACatalog(t *testing.T) {
	have := map[string]bool{}
	for _, l := range Languages() {
		have[l] = true
	}
	for _, l := range nonEnglish() {
		if !have[l] {
			t.Errorf("no catalog for %s", l)
		}
	}
}

func TestMessagesAreFoundInTheSource(t *testing.T) {
	messages, _ := collect(t)
	if len(messages) < 100 {
		t.Fatalf("found only %d messages; the scan is broken", len(messages))
	}
}

func TestDynamicMessagesAreAccountedFor(t *testing.T) {
	_, dynamic := collect(t)
	for key, locs := range dynamic {
		if _, ok := dynamicOK[key]; !ok {
			t.Errorf("%s is not a literal message (%s:%d).\n"+
				"  Translate it with msgcat.Mark / msgcat.T / a literal, or list it in dynamicOK with the reason it stays as is.",
				key, locs[0].file, locs[0].line)
		}
	}
	for key := range dynamicOK {
		if _, ok := dynamic[key]; !ok {
			t.Errorf("dynamicOK lists %q, which no longer appears in the source", key)
		}
	}
}

func TestEveryMessageIsTranslated(t *testing.T) {
	messages, _ := collect(t)
	for _, lang := range nonEnglish() {
		catalog := loadCatalogFile(t, lang)
		var missing []string
		for msg := range messages {
			tr, ok := catalog[msg]
			if !ok || strings.TrimSpace(tr) == "" {
				missing = append(missing, msg)
				continue
			}
			if !SameVerbs(msg, tr) {
				t.Errorf("%s: %q and its translation take different arguments: %q", lang, msg, tr)
			}
			if strings.ContainsRune(tr, '—') {
				t.Errorf("%s: translation of %q contains an em dash: %q", lang, msg, tr)
			}
			if strings.TrimSpace(tr) != tr {
				t.Errorf("%s: translation of %q has stray whitespace: %q", lang, msg, tr)
			}
			if strings.Count(msg, "\n") != strings.Count(tr, "\n") {
				t.Errorf("%s: %q and its translation differ in line breaks", lang, msg)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			shown := missing
			if len(shown) > 8 {
				shown = shown[:8]
			}
			t.Errorf("%s is missing %d of %d messages, for example:\n  %s",
				lang, len(missing), len(messages), strings.Join(shown, "\n  "))
		}
	}
}

func TestNoCatalogEntryIsUnused(t *testing.T) {
	messages, _ := collect(t)
	for _, lang := range nonEnglish() {
		var stale []string
		for msg := range loadCatalogFile(t, lang) {
			if _, ok := messages[msg]; !ok {
				stale = append(stale, msg)
			}
		}
		sort.Strings(stale)
		if len(stale) > 0 {
			t.Errorf("%s has %d entries no source message uses, for example %q", lang, len(stale), stale[0])
		}
	}
}

// Every message must parse as a template, or a typo in a verb would make the
// translation silently never match.
func TestMessagesParse(t *testing.T) {
	messages, _ := collect(t)
	for msg, locs := range messages {
		if !strings.Contains(msg, "%") {
			continue
		}
		if _, ok := parse(msg); !ok {
			// Allowed only as plain text: a bare percent sign such as "100% sure".
			if strings.Contains(msg, "%s") || strings.Contains(msg, "%d") || strings.Contains(msg, "%v") || strings.Contains(msg, "%q") {
				t.Errorf("%q (%s:%d) looks like a template but does not parse", msg, locs[0].file, locs[0].line)
			}
		}
	}
	_ = fmt.Sprint
}
