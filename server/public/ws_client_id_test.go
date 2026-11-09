package public

import (
	"net/http/httptest"
	"testing"
)

// The public client id (welcome._clientId, _peers, _sender) must not be the ?cid=
// reconnect credential: every peer sees the public id, and the hub treats a second
// connection with the same id as a takeover of the first.
func TestPublicClientIDIsNotTheReconnectSecret(t *testing.T) {
	req := func(cid string) string {
		r := httptest.NewRequest("GET", "/api/game/ws?cid="+cid, nil)
		return resolveClientID(r, 1, false, 0)
	}
	victimSecret := "c-victimsecret01"
	victimPublic := req(victimSecret)

	if victimPublic == victimSecret {
		t.Fatal("the public id equals the cid - any peer can read it off the roster and take the slot")
	}
	if req(victimSecret) != victimPublic {
		t.Fatal("the same cid must map to the same public id, or a refresh loses its slot")
	}
	if req(victimPublic) == victimPublic {
		t.Fatal("connecting with a victim's PUBLIC id as ?cid= reproduced it - the takeover is still open")
	}
	if req("c-othersecret02") == victimPublic {
		t.Fatal("two different cids collided on one public id")
	}
	if !validClientID(victimPublic) {
		t.Fatalf("derived id %q is not a valid client id", victimPublic)
	}

	// Scoped per project: another project never shares the id.
	r := httptest.NewRequest("GET", "/api/game/ws?cid="+victimSecret, nil)
	if resolveClientID(r, 2, false, 0) == victimPublic {
		t.Error("the same cid produced the same public id in a different project")
	}
	// Authenticated endpoints keep the account id.
	if got := resolveClientID(r, 1, true, 7); got != "u7" {
		t.Errorf("requires_auth id = %q, want u7", got)
	}
}
