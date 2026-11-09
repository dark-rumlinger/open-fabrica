package engine

// Drift guard: a project-facing event must actually be RAISED by something.
//
// trigger_events_drift_test.go asserts every project-facing constant in
// events/types.go is DOCUMENTED; this guard closes the other half. An event
// nobody raises is a trigger the model wires, the gate accepts, and production
// never fires: the most expensive shape of bug this platform has, because
// nothing looks wrong.
//
// LIMITATION - the check is per-PACKAGE, not per-call: publishers pass the type
// as a variable, so "is this constant the first argument to a NewEvent call" is
// mostly undecidable. What IS decidable is whether the constant is even
// mentioned in a package that publishes at all. A package that publishes some
// events while merely subscribing to others would pass this wrongly; that is
// the price of not being a runtime check.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/events"
)

// eventPkgFacts is what one package directory says about events: whether it raises
// any, and which event constants it names.
type eventPkgFacts struct {
	publishes bool
	refs      map[string]bool
}

// scanEventPackages walks non-test Go source and records, per directory, whether it
// calls Bus.Publish(...) and which events.Event* constants it mentions.
func scanEventPackages(t *testing.T) map[string]*eventPkgFacts {
	t.Helper()
	root := moduleRoot(t)
	out := map[string]*eventPkgFacts{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable dir is not this guard's business
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "data", "readme_assets":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		af, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		dir := filepath.Dir(p)
		f := out[dir]
		if f == nil {
			f = &eventPkgFacts{refs: map[string]bool{}}
			out[dir] = f
		}

		// A Subscribe argument does NOT count as naming the event, and skipping it
		// is what gives this guard teeth rather than the appearance of them:
		// bot/service.go both publishes (its own chat events) and subscribes to
		// payment.completed, so counting a Subscribe would have let the bot's
		// listener vouch for an event nobody raised - the precise confusion that
		// kept payment.completed looking alive for so long. Listening for a thing
		// is not evidence the thing happens.
		subscribed := map[token.Pos]bool{}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Subscribe" {
				return true
			}
			for _, a := range call.Args {
				if s, ok := a.(*ast.SelectorExpr); ok {
					subscribed[s.Pos()] = true
				}
			}
			return true
		})

		ast.Inspect(af, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Publish" {
					f.publishes = true
				}
			case *ast.SelectorExpr:
				// events.EventFoo - the qualified form every consumer outside
				// package events must use.
				if pkg, ok := node.X.(*ast.Ident); ok && pkg.Name == "events" &&
					strings.HasPrefix(node.Sel.Name, "Event") && !subscribed[node.Pos()] {
					f.refs[node.Sel.Name] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// projectFacingEventConsts maps each project-facing constant NAME to its wire value,
// parsed from events/types.go. Reuses projectFacingPrefixes so this guard and the
// documentation guard cannot disagree about which events a project's triggers see.
func projectFacingEventConsts(t *testing.T) map[string]string {
	t.Helper()
	src := readSource(t, "events/types.go")
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "types.go", src, 0)
	if err != nil {
		t.Fatalf("parsing events/types.go: %v", err)
	}
	out := map[string]string{}
	ast.Inspect(af, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val := strings.Trim(lit.Value, `"`)
		for _, p := range projectFacingPrefixes {
			if strings.HasPrefix(val, p) {
				out[vs.Names[0].Name] = val
			}
		}
		return true
	})
	return out
}

// TestProjectFacingEventsAreRaised asserts every event the trigger-events contract
// teaches is at least named by a package that publishes. The contract is the model's
// only catalogue of what it can hang a trigger on, so a name in it that nothing
// raises is a fulfillment path that silently never runs.
func TestProjectFacingEventsAreRaised(t *testing.T) {
	facing := projectFacingEventConsts(t)
	if len(facing) < 8 {
		t.Fatalf("found only %d project-facing event constants - the prefix filter may be wrong", len(facing))
	}
	pkgs := scanEventPackages(t)
	publishers := 0
	for _, f := range pkgs {
		if f.publishes {
			publishers++
		}
	}
	if publishers < 3 {
		t.Fatalf("found only %d publishing packages - the AST scan is not reading what it thinks it is", publishers)
	}

	for name, wire := range facing {
		raised := false
		for _, f := range pkgs {
			if f.publishes && f.refs[name] {
				raised = true
				break
			}
		}
		if !raised {
			t.Errorf("event %q (events.%s) is documented as a project-facing trigger event, but no publishing package even names it: a trigger on it can never fire", wire, name)
		}
	}
}

// TestEventTypeAliasesStayInSync corroborates that the constant names this guard
// reasons about still carry the wire values the contract documents. Cheap, and it
// fails loudly if a rename ever decouples the two.
func TestEventTypeAliasesStayInSync(t *testing.T) {
	for name, want := range map[string]string{
		string(events.EventPaymentCompleted):    "payment.completed",
		string(events.EventPaymentFailed):       "payment.failed",
		string(events.EventNotificationCreated): "notification.created",
		string(events.EventDataInsert):          "data.insert",
	} {
		if name != want {
			t.Errorf("event constant carries %q, want %q", name, want)
		}
	}
}
