package api

import (
	"testing"

	"openfabrica/store"
	"openfabrica/tools"
)

// TestAPICreateAuthoredPersistsScope pins auth_model=authored through to the
// stored row: owner_column is set (writes are scoped to the author) and
// owner_scope carries the authored marker (reads stay shared).
func TestAPICreateAuthoredPersistsScope(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{
		EndpointPath: "messages",
		TableName:    "items",
		AuthModel:    "authored",
		OwnerColumn:  "user_id",
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create authored: err=%v res=%v", err, res)
	}

	var ownerCol, ownerScope string
	var requiresAuth int
	if err := db.QueryRow(`SELECT COALESCE(owner_column,''), COALESCE(owner_scope,''), requires_auth FROM of_api_endpoints WHERE path='messages'`).
		Scan(&ownerCol, &ownerScope, &requiresAuth); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ownerCol != "user_id" {
		t.Errorf("owner_column=%q, want user_id", ownerCol)
	}
	if ownerScope != store.OwnerScopeAuthored {
		t.Errorf("owner_scope=%q, want %q", ownerScope, store.OwnerScopeAuthored)
	}
	if requiresAuth != 1 {
		t.Errorf("authored must require auth; requires_auth=%d", requiresAuth)
	}
}

// TestAPICreateAuthoredRequiresOwnerColumn rejects authored with no owner_column:
// the author column cannot be inferred and an empty one would silently leave the
// endpoint unscoped.
func TestAPICreateAuthoredRequiresOwnerColumn(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{EndpointPath: "messages", TableName: "items", AuthModel: "authored"}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("authored without owner_column should fail; res=%v", res)
	}
}

// TestAPICreateAuthoredRejectsPublicWrite confirms the same public_write
// incompatibility owner has: an anonymous writer has no identity to author a row.
func TestAPICreateAuthoredRejectsPublicWrite(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{
		EndpointPath:  "messages",
		TableName:     "items",
		AuthModel:     "authored",
		OwnerColumn:   "user_id",
		WriteIsPublic: true,
	}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("authored + public_write should fail; res=%v", res)
	}
}

// TestAPIUpdateAuthoredClearsScopeOnSwitch confirms owner_scope is rewritten by an
// auth_model change: authored sets it, and switching back to owner clears it so the
// endpoint returns to scoping reads too. A stale owner_scope would keep reads shared
// after the model asked for private-per-user.
func TestAPIUpdateAuthoredClearsScopeOnSwitch(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, err := (&APICreate{EndpointPath: "messages", TableName: "items", AuthModel: "authored", OwnerColumn: "user_id"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("create authored: err=%v res=%v", err, res)
	}
	if res, err := (&APIUpdate{EndpointPath: "messages", AuthModel: "owner", OwnerColumn: "user_id"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("update to owner: err=%v res=%v", err, res)
	}

	var ownerScope string
	if err := db.QueryRow(`SELECT COALESCE(owner_scope,'') FROM of_api_endpoints WHERE path='messages'`).Scan(&ownerScope); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ownerScope != "" {
		t.Errorf("owner_scope=%q after switching to owner, want empty (reads scoped again)", ownerScope)
	}
}
