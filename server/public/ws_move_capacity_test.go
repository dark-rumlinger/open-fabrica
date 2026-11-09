package public

import "testing"

// A player switching rooms in-band must not overfill a room the initial join gate
// would have capped: moveClient recomputes capacity at the destination and demotes
// the mover to spectator when the room is already at max_clients.
func TestMoveClientDemotesIntoFullRoom(t *testing.T) {
	h := NewWSHub()
	toKey := "1:0:ep:arena"
	full := &wsRoom{clients: map[string]*wsClient{}}
	full.clients["occ"] = newWSClient("occ", nil)
	h.rooms[toKey] = full

	fromKey := "1:0:ep:lobby"
	mover := newWSClient("mv", nil)
	h.rooms[fromKey] = &wsRoom{clients: map[string]*wsClient{"mv": mover}}

	if _, ok := h.moveClient(mover, fromKey, "lobby", toKey, "arena", 1); !ok {
		t.Fatal("moveClient should succeed")
	}
	if !mover.spectator {
		t.Fatal("a player moving into a room already at max_clients must be demoted to spectator")
	}
}

// The reverse: moving into a room with spare capacity keeps the mover a player.
func TestMoveClientKeepsPlayerWhenRoomHasSpace(t *testing.T) {
	h := NewWSHub()
	toKey := "1:0:ep:arena"
	h.rooms[toKey] = &wsRoom{clients: map[string]*wsClient{
		"occ": {id: "occ", ch: make(chan wsMessage, 8)},
	}}
	fromKey := "1:0:ep:lobby"
	mover := newWSClient("mv", nil)
	h.rooms[fromKey] = &wsRoom{clients: map[string]*wsClient{"mv": mover}}

	if _, ok := h.moveClient(mover, fromKey, "lobby", toKey, "arena", 4); !ok {
		t.Fatal("moveClient should succeed")
	}
	if mover.spectator {
		t.Fatal("a player moving into a room with spare capacity must stay a player")
	}
}
