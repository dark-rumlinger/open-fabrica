package validators

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"openfabrica/internal/flowsnap"
	"openfabrica/probe"
	"openfabrica/store"
)

// The missing-file gate needs no browser: any project with a page and no
// of_test.js is flagged before anything is launched; a headless API is left alone.
func TestFlowTests_MissingFileOnProjectWithPagesIsAnError(t *testing.T) {
	cases := map[string]struct {
		kind, path, body string
		want             bool
	}{
		"form page":         {"page", "/signup", `<form><input name="email"></form>`, true},
		"button page":       {"page", "/", `<button id="start">Start</button>`, true},
		"canvas game":       {"page", "/", `<canvas id="game"></canvas>`, true},
		"static about page": {"page", "/about", `<h1>About us</h1><p>We make things.</p>`, true},
		"headless api":      {"file", "openapi.json", `{}`, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			db := setupAuthPageDB(t)
			savePageMeta(t, db, c.kind, c.path, c.body, "{}")
			issues := flowTestsValidator{}.Validate(context.Background(), db, "http://127.0.0.1:1", "")
			if !c.want {
				if len(issues) != 0 {
					t.Fatalf("headless project flagged: %+v", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0].Signature != "flow|missing" || issues[0].Severity != "error" || !issues[0].Deterministic {
				t.Fatalf("want one deterministic flow|missing error, got %+v", issues)
			}
			if !strings.Contains(issues[0].Message, "write_flows") {
				t.Fatalf("message must name the fix: %s", issues[0].Message)
			}
		})
	}
}

// A headless API with endpoints owes request-step journeys; the message says so.
func TestFlowTests_MissingFileOnAPIProjectIsAnError(t *testing.T) {
	db := setupAuthPageDB(t)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS of_api_endpoints (id INTEGER PRIMARY KEY, path TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path) VALUES ('orders')`); err != nil {
		t.Fatal(err)
	}
	issues := flowTestsValidator{}.Validate(context.Background(), db, "http://127.0.0.1:1", "")
	if len(issues) != 1 || issues[0].Signature != "flow|missing" || !strings.Contains(issues[0].Message, "request steps") {
		t.Fatalf("want the API flow|missing error, got %+v", issues)
	}
}

func TestFlowTests_InvalidStoredScriptIsAnError(t *testing.T) {
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "test", "of_test.js", `export default { flows: [ }`, "{}")
	issues := flowTestsValidator{}.Validate(context.Background(), db, "http://127.0.0.1:1", "")
	if len(issues) != 1 || issues[0].Signature != "flow|parse" {
		t.Fatalf("want a flow|parse error, got %+v", issues)
	}
}

// With of_test.js saved but no Chrome on the host, the validator reports a
// warning (never an error, so no fix-pass) instead of failing the app.
func TestFlowTests_NoChromeIsAWarningNotAnError(t *testing.T) {
	defer probe.SetFlowBrowserLauncherForTest(func(context.Context, store.BrowserConfig) (context.Context, context.CancelFunc, error) {
		return nil, nil, fmt.Errorf("%w: no chrome", store.ErrBrowserUnavailable)
	})()
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "page", "/", `<button id="go">Go</button>`, "{}")
	savePageMeta(t, db, "test", "of_test.js", `export default { flows: [{ name: "a", steps: [{ goto: "/" }, { click: "#go" }] }] }`, "{}")
	issues := flowTestsValidator{}.Validate(context.Background(), db, "http://127.0.0.1:1", "")
	if len(issues) != 1 || issues[0].Severity != store.SeverityWarning || issues[0].Deterministic || !strings.Contains(issues[0].Message, "skipped") {
		t.Fatalf("want one non-deterministic skip warning, got %+v", issues)
	}
	if issues[0].Signature != probe.FlowSkippedSignaturePrefix+probe.SkipNoBrowser {
		t.Fatalf("the skip must carry its kind in the signature, got %q", issues[0].Signature)
	}
}

// When the engine cannot snapshot the data (or the traffic guard says no), the
// replay is skipped with a warning: the app is never replayed against without a
// way to roll the replay's writes back.
func TestFlowTests_NoSnapshotMeansNoReplay(t *testing.T) {
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "page", "/", `<button id="go">Go</button>`, "{}")
	savePageMeta(t, db, "test", "of_test.js", `export default { flows: [{ name: "a", steps: [{ goto: "/" }, { click: "#go" }] }] }`, "{}")
	kinds := map[string]string{"snapshot failed": probe.SkipSnapshot, "guard said no": probe.SkipVisitors}
	for name, open := range map[string]flowsnap.Opener{
		"snapshot failed": func(context.Context) (*flowsnap.Snapshot, string, error) { return nil, "", fmt.Errorf("disk full") },
		"guard said no": func(context.Context) (*flowsnap.Snapshot, string, error) {
			return nil, "real visitors used the app", nil
		},
	} {
		issues := flowTestsValidator{}.Validate(flowsnap.WithOpenerFunc(context.Background(), open), db, "http://127.0.0.1:1", "")
		if len(issues) != 1 || issues[0].Severity != store.SeverityWarning || issues[0].Deterministic || !strings.Contains(issues[0].Message, "skipped") {
			t.Fatalf("%s: want one skip warning, got %+v", name, issues)
		}
		if want := probe.FlowSkippedSignaturePrefix + kinds[name]; issues[0].Signature != want {
			t.Fatalf("%s: signature = %q, want %q", name, issues[0].Signature, want)
		}
	}
}
