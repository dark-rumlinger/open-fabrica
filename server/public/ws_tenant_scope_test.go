package public

import (
	"testing"

	"openfabrica/events"
)

// A signed-in player's JWT tenant must NOT shard a WebSocket room on an endpoint
// that never opted into tenancy (no tenant_column). With signup_creates_tenant
// every player carries a distinct tenant, so folding it into the room key
// unconditionally would put each player in a room of their own, and multiplayer
// players could not see each other. effectiveRoomTenant
// gates the fold on tenant_column, matching apiWSRooms and the ws_broadcast
// trigger - so two different-tenant players (and an anonymous one) share ONE
// room key and therefore see each other.
func TestEffectiveRoomTenantSharesNonTenantedRooms(t *testing.T) {
	const projectID = 1
	const path, room = "game", "world"
	nonTenanted := &wsEndpointConfig{} // no TenantColumn

	keyTenant1 := events.RoomKey(projectID, effectiveRoomTenant(nonTenanted, 1), path, room)
	keyTenant2 := events.RoomKey(projectID, effectiveRoomTenant(nonTenanted, 2), path, room)
	keyAnon := events.RoomKey(projectID, effectiveRoomTenant(nonTenanted, 0), path, room)
	if keyTenant1 != keyTenant2 || keyTenant1 != keyAnon {
		t.Fatalf("non-tenanted endpoint sharded the room by JWT tenant: t1=%q t2=%q anon=%q (all must match)", keyTenant1, keyTenant2, keyAnon)
	}
	if got := effectiveRoomTenant(nonTenanted, 7); got != 0 {
		t.Fatalf("non-tenanted endpoint must scope to tenant 0, got %d", got)
	}
}

// A tenant_column endpoint is genuinely multi-tenant: its rooms MUST stay
// isolated per tenant, so the visibility fix must not leak one tenant's room
// into another's.
func TestEffectiveRoomTenantIsolatesTenantedRooms(t *testing.T) {
	const projectID = 1
	const path, room = "board", "main"
	tenanted := &wsEndpointConfig{TenantColumn: "tenant_id"}

	keyTenant1 := events.RoomKey(projectID, effectiveRoomTenant(tenanted, 1), path, room)
	keyTenant2 := events.RoomKey(projectID, effectiveRoomTenant(tenanted, 2), path, room)
	if keyTenant1 == keyTenant2 {
		t.Fatalf("tenant_column endpoint must isolate rooms per tenant, but both got %q", keyTenant1)
	}
	if got := effectiveRoomTenant(tenanted, 5); got != 5 {
		t.Fatalf("tenant_column endpoint must preserve the real tenant, got %d", got)
	}
}
