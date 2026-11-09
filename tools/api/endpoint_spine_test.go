package api

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/internal/endpoints"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// spineOnlyDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func spineOnlyDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

// TestClaimEndpoint_MethodAwareCoLocationAndCollision pins the spine's core
// contract: a function and a CRUD endpoint can share a bare path on DIFFERENT
// methods, but a same-method double-claim across kinds is rejected. A sub-route
// kind (upload) never collides with the bare path. Release frees the route.
func TestClaimEndpoint_MethodAwareCoLocationAndCollision(t *testing.T) {
	db := spineOnlyDB(t)
	ctx := &tools.ToolContext{DB: db}

	if err := ClaimEndpoint(ctx, endpoints.KindFunction, "files", []string{"POST"}); err != nil {
		t.Fatalf("function POST claim: %v", err)
	}
	// GET is free -> a CRUD GET lookup co-locates on the same bare path.
	if err := ClaimEndpoint(ctx, endpoints.KindCRUD, "files", []string{"GET"}); err != nil {
		t.Fatalf("method-aware co-location should be allowed: %v", err)
	}
	// POST is taken by the function -> a CRUD POST at the same path collides.
	err := ClaimEndpoint(ctx, endpoints.KindCRUD, "files", []string{"POST"})
	if err == nil {
		t.Fatal("expected a collision claiming POST already owned by the function")
	}
	if !strings.Contains(err.Error(), "function") {
		t.Errorf("collision error should name the owning kind; got %q", err)
	}
	// A sub-route kind occupies a different sub_segment -> no collision.
	if err := ClaimEndpoint(ctx, endpoints.KindUpload, "files", nil); err != nil {
		t.Fatalf("upload sub-route should not collide with the bare path: %v", err)
	}
	// Same-kind re-claim is idempotent (build resume).
	if err := ClaimEndpoint(ctx, endpoints.KindFunction, "files", []string{"POST"}); err != nil {
		t.Fatalf("idempotent re-claim: %v", err)
	}
	// Release frees the function's POST so another kind can take it.
	if err := ReleaseEndpoint(ctx, endpoints.KindFunction, "files"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := ClaimEndpoint(ctx, endpoints.KindCRUD, "files", []string{"POST"}); err != nil {
		t.Fatalf("after release, POST should be claimable: %v", err)
	}
}

// TestClaimEndpoint_RejectsReservedSuffixPath pins the naming guard: a path whose
// last segment is the suffix the runtime appends for that kind is rejected, so an
// upload endpoint cannot be created at /api/upload/upload (the trap that invites a
// page to POST the bare /api/upload). A different path, and a CRUD endpoint whose
// bare path happens to be a reserved word, are both fine.
func TestClaimEndpoint_RejectsReservedSuffixPath(t *testing.T) {
	db := spineOnlyDB(t)
	ctx := &tools.ToolContext{DB: db}

	if err := ClaimEndpoint(ctx, endpoints.KindUpload, "upload", nil); err == nil {
		t.Fatal("expected rejection: an upload endpoint named 'upload' serves /api/upload/upload")
	} else if !strings.Contains(err.Error(), "runtime suffix") {
		t.Errorf("guard error should explain the suffix collision; got %q", err)
	}
	if err := ClaimEndpoint(ctx, endpoints.KindLLM, "chat", nil); err == nil {
		t.Fatal("expected rejection: an LLM proxy named 'chat' serves /api/chat/chat")
	}
	// A non-reserved path is accepted.
	if err := ClaimEndpoint(ctx, endpoints.KindUpload, "avatars", nil); err != nil {
		t.Fatalf("upload endpoint at a normal path should be allowed: %v", err)
	}
	// CRUD answers the bare path, so the suffix words are not reserved for it.
	if err := ClaimEndpoint(ctx, endpoints.KindCRUD, "upload", []string{"GET"}); err != nil {
		t.Fatalf("a CRUD endpoint named 'upload' is fine (it serves the bare path): %v", err)
	}
}

func TestEndpointRoutesFor(t *testing.T) {
	cases := map[string][]endpointRoute{
		endpoints.KindUpload: {{"upload", "POST"}},
		endpoints.KindSSE:    {{"stream", "GET"}},
		endpoints.KindLLM:    {{"chat", "POST"}, {"complete", "POST"}},
		endpoints.KindWS:     {{"ws", "*"}, {"rooms", "GET"}},
		endpoints.KindExport: {{"export", "GET"}},
		endpoints.KindImport: {{"import", "POST"}},
		endpoints.KindQuery:  {{"", "POST"}},
	}
	for kind, want := range cases {
		got := endpointRoutesFor(kind, nil)
		if len(got) != len(want) {
			t.Errorf("%s: got %d routes, want %d (%v)", kind, len(got), len(want), got)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s route %d: got %+v, want %+v", kind, i, got[i], want[i])
			}
		}
	}
	// crud/function expand per method.
	if got := endpointRoutesFor(endpoints.KindCRUD, []string{"GET", "POST"}); len(got) != 2 || got[0] != (endpointRoute{"", "GET"}) {
		t.Errorf("crud per-method routes: %+v", got)
	}
}

// TestEndpointKindsAreFullyWired pins the dual-registry invariant behind the
// of_endpoints spine: every per-kind config table in endpointTableKind must also
// have a non-empty route set in endpointRoutesFor. The spine is a denormalized
// routing index kept in step with the config tables on write, and endpointRoutesFor
// is the single source of truth for a kind's routes. Adding a new endpoint kind's
// table (and its delete-release mapping here) but forgetting its routes would make
// every created endpoint of that kind claim zero spine rows and 404 at runtime -
// exactly the kind of silent desync the two-table design risks. This fails the
// build the moment the two maps drift.
func TestEndpointKindsAreFullyWired(t *testing.T) {
	for table, kind := range endpointTableKind {
		// GET+POST covers crud/function (the only method-varying kinds); fixed-route
		// kinds ignore the methods argument.
		if routes := endpointRoutesFor(kind, []string{"GET", "POST"}); len(routes) == 0 {
			t.Errorf("%s maps to kind %q, but endpointRoutesFor returns no routes for it: "+
				"a created endpoint would claim no spine rows and 404 - wire its routes in endpointRoutesFor", table, kind)
		}
	}
}
