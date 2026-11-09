package public

import (
	"fmt"
	"sync"
	"testing"

	"openfabrica/db"
	"openfabrica/events"
)

// SSE pushes a row event's FIELDS, so a stream over an owner- or tenant-scoped
// table hands every subscriber every other user's and tenant's rows unless the
// subscription is filtered. The REST path closes that with a SQL predicate and the
// WS path by folding the tenant into the room key; the stream inherits the same
// decision from the endpoint that published the row.

// scopeFixture registers one API endpoint and returns a scope for a given identity.
func scopeFixture(t *testing.T, endpointPath, ownerCol, tenantCol string, publicRead bool) *db.ProjectDB {
	t.Helper()
	return scopeFixtureAuth(t, endpointPath, ownerCol, tenantCol, publicRead, true, "")
}

// scopeFixtureAuth is scopeFixture with the endpoint's own access gate spelled out:
// whether it requires auth and which role it requires.
func scopeFixtureAuth(t *testing.T, endpointPath, ownerCol, tenantCol string, publicRead, requiresAuth bool, requiredRole string) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, public_read, owner_column, tenant_column, required_role)
		 VALUES (?, 'notes', '["GET","POST"]', ?, ?, ?, ?, ?)`,
		endpointPath, requiresAuth, publicRead, ownerCol, tenantCol, requiredRole); err != nil {
		t.Fatalf("register endpoint: %v", err)
	}
	return pdb
}

func newScope(pdb *db.ProjectDB, userID, tenantID int, role, tenantRole string) *streamScope {
	return &streamScope{
		h:          &Handler{deps: &Deps{}},
		projectDB:  pdb.Reader(),
		userID:     userID,
		userRole:   role,
		tenantID:   tenantID,
		tenantRole: tenantRole,
		cache:      map[string]*apiEndpoint{},
	}
}

func TestStreamScopeOwnerColumn(t *testing.T) {
	pdb := scopeFixture(t, "notes", "user_id", "", false)
	mine := map[string]any{"endpoint": "notes", "table": "notes", "id": 1, "user_id": float64(7), "body": "mine"}
	theirs := map[string]any{"endpoint": "notes", "table": "notes", "id": 2, "user_id": float64(8), "body": "theirs"}

	me := newScope(pdb, 7, 0, "", "")
	if !me.allow(events.EventDataInsert, mine) {
		t.Error("a subscriber must still receive its own rows")
	}
	if me.allow(events.EventDataInsert, theirs) {
		t.Error("an owner-scoped stream leaked another user's row")
	}

	anon := newScope(pdb, 0, 0, "", "")
	if anon.allow(events.EventDataInsert, mine) {
		t.Error("an unauthenticated subscriber must receive nothing from an owner-scoped endpoint")
	}

	admin := newScope(pdb, 9, 0, "admin", "")
	if !admin.allow(events.EventDataInsert, theirs) {
		t.Error("the admin role bypasses owner scoping on REST, so it must here too")
	}

	key := newScope(pdb, 0, 0, "", "")
	key.isProjectKey = true
	if !key.allow(events.EventDataInsert, theirs) {
		t.Error("a project API key has owner-level access, so it must here too")
	}
}

func TestStreamScopeTenantColumn(t *testing.T) {
	pdb := scopeFixture(t, "notes", "", "tenant_id", false)
	tenantA := map[string]any{"endpoint": "notes", "table": "notes", "id": 1, "tenant_id": int64(1)}
	tenantB := map[string]any{"endpoint": "notes", "table": "notes", "id": 2, "tenant_id": int64(2)}

	inA := newScope(pdb, 5, 1, "", "")
	if !inA.allow(events.EventDataInsert, tenantA) {
		t.Error("a tenant must still receive its own rows")
	}
	if inA.allow(events.EventDataInsert, tenantB) {
		t.Error("a tenant-scoped stream leaked another tenant's row")
	}

	noTenant := newScope(pdb, 5, 0, "", "")
	if noTenant.allow(events.EventDataInsert, tenantA) {
		t.Error("a session with no active tenant must receive nothing from a tenanted endpoint")
	}

	platform := newScope(pdb, 5, 0, "", "platform_admin")
	if !platform.allow(events.EventDataInsert, tenantB) {
		t.Error("platform_admin bypasses tenancy on REST, so it must here too")
	}
}

// A payload that does not carry the scoping column cannot be attributed to anyone.
// A delete event is exactly that shape (table, endpoint, id and nothing else), so
// it has to fail closed rather than fan out to every subscriber.
func TestStreamScopeUnattributablePayloadFailsClosed(t *testing.T) {
	pdb := scopeFixture(t, "notes", "user_id", "", false)
	del := map[string]any{"endpoint": "notes", "table": "notes", "id": 2}
	if newScope(pdb, 7, 0, "", "").allow(events.EventDataInsert, del) {
		t.Error("a payload with no owner column must not be delivered on an owner-scoped stream")
	}
}

// An unscoped endpoint streams to everyone, and a public_read endpoint keeps the
// unscoped reads it opted into: the stream must not narrow a deliberately public
// feed.
func TestStreamScopeUnscopedAndPublicReadPassThrough(t *testing.T) {
	open := scopeFixtureAuth(t, "posts", "", "", false, false, "")
	row := map[string]any{"endpoint": "posts", "table": "posts", "id": 1, "title": "hello"}
	if !newScope(open, 0, 0, "", "").allow(events.EventDataInsert, row) {
		t.Error("a public endpoint with no owner/tenant column must stream to every subscriber")
	}

	pub := scopeFixture(t, "feed", "user_id", "", true)
	pubRow := map[string]any{"endpoint": "feed", "table": "posts", "id": 1, "user_id": float64(3)}
	if !newScope(pub, 0, 0, "", "").allow(events.EventDataInsert, pubRow) {
		t.Error("public_read exposes unscoped reads on purpose, so the stream must match")
	}
}

// The stream must clear the same read gate REST applies: an endpoint that requires
// auth withholds its rows from a subscriber with no identity even when it has no
// owner or tenant column, and a required_role holds on the stream too. A project key
// and the admin role keep their bypass.
func TestStreamScopeHonoursEndpointAuthGate(t *testing.T) {
	gated := scopeFixtureAuth(t, "posts", "", "", false, true, "")
	row := map[string]any{"endpoint": "posts", "table": "posts", "id": 1, "title": "hello"}
	if newScope(gated, 0, 0, "", "").allow(events.EventDataInsert, row) {
		t.Error("an auth-gated endpoint leaked a row to a subscriber with no identity")
	}
	if !newScope(gated, 7, 0, "", "").allow(events.EventDataInsert, row) {
		t.Error("an authenticated subscriber must receive rows of an auth-gated endpoint")
	}
	key := newScope(gated, 0, 0, "", "")
	key.isProjectKey = true
	if !key.allow(events.EventDataInsert, row) {
		t.Error("a project API key has owner-level access, so it must here too")
	}

	roled := scopeFixtureAuth(t, "reports", "", "", false, true, "editor")
	rrow := map[string]any{"endpoint": "reports", "table": "reports", "id": 1}
	if newScope(roled, 7, 0, "member", "").allow(events.EventDataInsert, rrow) {
		t.Error("a subscriber without the endpoint's required_role must not receive its rows")
	}
	if !newScope(roled, 7, 0, "editor", "").allow(events.EventDataInsert, rrow) {
		t.Error("a subscriber holding the required_role must receive the rows")
	}
	if !newScope(roled, 9, 0, "admin", "").allow(events.EventDataInsert, rrow) {
		t.Error("admin satisfies every role gate on REST, so it must here too")
	}
}

// A non-row event with no endpoint (a job, a webhook, a custom event name) has no
// scoping decision to inherit and passes. A data.* event with no endpoint is a
// different thing: the CRUD path always stamps one, so an unattributed row event
// is a custom publish, and passing it would route rows straight past the
// owner/tenant filter. One naming an endpoint that no longer exists cannot be
// checked at all and also fails closed.
func TestStreamScopeUnknownAndEndpointlessEvents(t *testing.T) {
	pdb := scopeFixture(t, "notes", "user_id", "", false)
	s := newScope(pdb, 7, 0, "", "")
	if !s.allow("job.finished", map[string]any{"table": "notes", "id": 1}) {
		t.Error("a non-row event with no endpoint is out of scope for endpoint-derived filtering")
	}
	if s.allow(events.EventDataInsert, map[string]any{"table": "notes", "id": 1}) {
		t.Error("an unattributable row event must fail closed on a scoped stream")
	}
	if s.allow(events.EventDataInsert, map[string]any{"endpoint": "gone", "table": "notes", "id": 1}) {
		t.Error("an event from an endpoint we cannot resolve must fail closed")
	}
}

// The bus delivers each event to a subscriber inline on the publishing
// goroutine, and publishes arrive from many goroutines at once, so one SSE
// subscription's scope check runs concurrently with itself. endpointFor memoises
// into a shared map, so without a lock a concurrent cache miss is a concurrent map
// write - a fatal runtime error recover() cannot catch that takes the whole
// process down. Run under -race; it must stay clean.
func TestStreamScopeConcurrentEndpointCache(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	const endpoints = 8
	for i := 0; i < endpoints; i++ {
		if _, err := pdb.Writer().Exec(
			`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, public_read, owner_column, tenant_column)
			 VALUES (?, 'notes', '["GET"]', 0, 0, '', '')`,
			fmt.Sprintf("notes%d", i)); err != nil {
			t.Fatalf("register endpoint %d: %v", i, err)
		}
	}

	// One scope shared across workers, cache starting empty so the first burst all
	// races the miss path - the exact concurrent read-modify-write the lock guards.
	s := newScope(pdb, 0, 0, "", "")

	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				path := fmt.Sprintf("notes%d", (w+i)%endpoints)
				s.allow(events.EventDataInsert, map[string]any{"endpoint": path, "table": "notes", "id": i})
			}
		}(w)
	}
	close(start) // release every worker at once to widen the race window
	wg.Wait()
}

func TestSameIDNormalisesPayloadNumberShapes(t *testing.T) {
	for _, v := range []any{float64(7), int(7), int64(7), int32(7), "7"} {
		if !sameID(v, 7) {
			t.Errorf("sameID(%T %v, 7) = false, want true", v, v)
		}
	}
	for _, v := range []any{float64(8), "8", nil, "abc", true} {
		if sameID(v, 7) {
			t.Errorf("sameID(%T %v, 7) = true, want false", v, v)
		}
	}
}
