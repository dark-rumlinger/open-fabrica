package content

// Attaching a scope=global file must not silently take it off every other page.
//
// Attach declares a file page-scope, so a file saved GLOBAL and then attached carries
// two contradictory declarations. Resolving that toward the attach costs nothing when
// the project has ONE page - global and page ship identically there. With a second
// page it is destructive: the demotion pulls the shared stylesheet off every page but
// the attached one, no gate reports the unstyled pages, and the save path's own clobber
// gate already refuses the analogous thing ("landing it would take the global file off
// every page that depends on it, and nothing in this turn would report the breakage").
//
// A scope=page file is never in question - attach is exactly what it is for.

import (
	"strings"
	"testing"

	"openfabrica/store"
)

func TestAttachRefusesToDemoteGlobalWhenOtherPagesExist(t *testing.T) {
	ctx := setupClobberDB(t)
	for _, p := range []string{"/", "/world"} {
		if res, err := (&ArtSave{Kind: "page", FilePath: p, Body: "<h1>hi</h1>"}).Run(ctx); err != nil || !res.Success {
			t.Fatalf("seed page %s: err=%v res=%+v", p, err, res)
		}
	}
	// The shared design system, saved global exactly as the mental model prescribes.
	if res, err := (&ArtSave{Kind: "file", FilePath: "world.css", Body: ".btn{color:red}\n", Scope: "global"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed global css: err=%v res=%+v", err, res)
	}

	res, err := (&FileAttach{FilePath: "world.css", PagePath: "/world"}).Run(ctx)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if res.Success {
		t.Fatal("attaching a scope=global stylesheet succeeded while another page exists - " +
			"that demotes it to page scope and silently unstyles every other page")
	}
	if !strings.Contains(res.Error, "/") || !strings.Contains(res.Error, "scope=global") {
		t.Errorf("the refusal must name the scope and the pages that would lose the file, got: %s", res.Error)
	}

	// The decisive assertion: the file must STILL be global, so every page keeps it.
	if s := store.FileScope(ctx.DB, "world.css"); s != "global" {
		t.Errorf("world.css scope = %q after a refused attach, want \"global\" - the refusal must not "+
			"have demoted it anyway", s)
	}
}

// The sole-page case stays free: nothing can lose a file that is the only page's.
func TestAttachDemotesGlobalWhenItIsTheOnlyPage(t *testing.T) {
	ctx := setupClobberDB(t)
	if res, err := (&ArtSave{Kind: "page", FilePath: "/", Body: "<h1>hi</h1>"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page: err=%v res=%+v", err, res)
	}
	if res, err := (&ArtSave{Kind: "file", FilePath: "app.css", Body: ".btn{color:red}\n", Scope: "global"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed global css: err=%v res=%+v", err, res)
	}

	res, err := (&FileAttach{FilePath: "app.css", PagePath: "/"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("attach on a sole-page project must land: err=%v res=%+v", err, res)
	}
	if s := store.FileScope(ctx.DB, "app.css"); s != "page" {
		t.Errorf("app.css scope = %q, want \"page\" - with one page the correction is free and should still happen", s)
	}
}

// A page-scope file attaches freely regardless of how many pages exist.
func TestAttachPageScopedFileIsUnaffected(t *testing.T) {
	ctx := setupClobberDB(t)
	for _, p := range []string{"/", "/world"} {
		if res, err := (&ArtSave{Kind: "page", FilePath: p, Body: "<h1>hi</h1>"}).Run(ctx); err != nil || !res.Success {
			t.Fatalf("seed page %s: err=%v res=%+v", p, err, res)
		}
	}
	if res, err := (&ArtSave{Kind: "file", FilePath: "world.page.css", Body: ".x{color:red}\n", Scope: "page"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("seed page css: err=%v res=%+v", err, res)
	}
	res, err := (&FileAttach{FilePath: "world.page.css", PagePath: "/world"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("attaching a scope=page file must always land: err=%v res=%+v", err, res)
	}
}
