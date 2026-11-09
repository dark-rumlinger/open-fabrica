package public

import (
	"testing"

	"openfabrica/db"
)

// TestRoomJoinGate pins the per-room access-token entry gate: the first peer
// claims an unclaimed room and is handed a minted token, later peers must
// present a matching token, a missing/wrong/cross-room token is refused, the
// global room is never gated, and a token is reclaimable after the room empties.
func TestRoomJoinGate(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	h := &Handler{}
	gate := func(room, presented string) (string, bool, bool) {
		return h.roomJoinGate(pdb, "signal", 0, room, presented)
	}

	t.Run("global room is never gated", func(t *testing.T) {
		if _, _, ok := gate("", ""); !ok {
			t.Fatal("global room ('') must pass without a token")
		}
	})

	var alphaTok string
	t.Run("first peer claims and is handed a token", func(t *testing.T) {
		tok, claimer, ok := gate("alpha", "")
		if !ok || !claimer || tok == "" {
			t.Fatalf("first peer should claim: tok=%q claimer=%v ok=%v", tok, claimer, ok)
		}
		alphaTok = tok
	})

	t.Run("second peer with the right token passes as non-claimer", func(t *testing.T) {
		_, claimer, ok := gate("alpha", alphaTok)
		if !ok || claimer {
			t.Fatalf("matching token should pass as non-claimer: claimer=%v ok=%v", claimer, ok)
		}
	})

	t.Run("missing token is refused", func(t *testing.T) {
		if _, _, ok := gate("alpha", ""); ok {
			t.Fatal("a claimed room must refuse a peer with no token")
		}
	})

	t.Run("wrong token is refused", func(t *testing.T) {
		if _, _, ok := gate("alpha", "not-the-token-aaaaaaaaaaaaaaaaaaaa"); ok {
			t.Fatal("a wrong token must be refused")
		}
	})

	t.Run("cross-room token is refused", func(t *testing.T) {
		betaTok, claimer, ok := gate("beta", "")
		if !ok || !claimer || betaTok == alphaTok {
			t.Fatalf("beta should get its own independent claim: tok=%q claimer=%v ok=%v", betaTok, claimer, ok)
		}
		if _, _, ok := gate("beta", alphaTok); ok {
			t.Fatal("alpha's token must not open beta")
		}
	})

	t.Run("reclaimable after prune, and a held link re-establishes the same token", func(t *testing.T) {
		ws := &wsEndpointConfig{RoomGated: true, Path: "signal"}
		pruneRoomToken(pdb, ws, "alpha", 0)
		// A peer that still holds the old invite link re-establishes the SAME
		// token on the now-unclaimed room (the creator-reload robustness path).
		tok, claimer, ok := gate("alpha", alphaTok)
		if !ok || !claimer || tok != alphaTok {
			t.Fatalf("held link should re-establish same token after prune: tok=%q claimer=%v ok=%v", tok, claimer, ok)
		}
		// And a fresh claim with no token after prune mints a new one.
		pruneRoomToken(pdb, ws, "alpha", 0)
		fresh, claimer, ok := gate("alpha", "")
		if !ok || !claimer || fresh == alphaTok {
			t.Fatalf("fresh claim after prune should mint a new token: tok=%q claimer=%v ok=%v", fresh, claimer, ok)
		}
	})

	t.Run("tenants claim the same room name independently", func(t *testing.T) {
		t1, _, ok1 := h.roomJoinGate(pdb, "signal", 1, "shared", "")
		t2, _, ok2 := h.roomJoinGate(pdb, "signal", 2, "shared", "")
		if !ok1 || !ok2 || t1 == t2 {
			t.Fatalf("per-tenant claims must be independent: t1=%q t2=%q", t1, t2)
		}
		// Tenant 2's token must not satisfy tenant 1's room.
		if _, _, ok := h.roomJoinGate(pdb, "signal", 1, "shared", t2); ok {
			t.Fatal("a token from another tenant must be refused")
		}
	})
}
