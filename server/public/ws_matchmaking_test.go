package public

import (
	"encoding/json"
	"testing"
	"time"
)

func mkWaitingClient(id string, rating float64, region string, waitedSecs float64) *wsClient {
	meta, _ := json.Marshal(map[string]any{"rating": rating, "region": region})
	c := newWSClient(id, meta)
	// Backdate the join so the band-widening math sees a peer that has waited.
	c.joinedAt = time.Now().Add(-time.Duration(waitedSecs * float64(time.Second)))
	return c
}

func hubWithLobby(clients ...*wsClient) (*wsHub, string) {
	h := NewWSHub()
	key := "1:mm:lobby"
	room := &wsRoom{clients: map[string]*wsClient{}}
	for _, c := range clients {
		room.clients[c.id] = c
	}
	h.rooms[key] = room
	return h, key
}

func inGroup(group []string, id string) bool {
	for _, g := range group {
		if g == id {
			return true
		}
	}
	return false
}

func TestFormMatchNoCriteria(t *testing.T) {
	h, key := hubWithLobby(
		mkWaitingClient("a", 0, "", 3),
		mkWaitingClient("b", 0, "", 2),
		mkWaitingClient("c", 0, "", 1),
	)
	group, room, ok := h.formMatch(key, matchCriteria{GroupSize: 2})
	if !ok || len(group) != 2 || room == "" {
		t.Fatalf("expected a 2-peer match, got ok=%v group=%v room=%q", ok, group, room)
	}
}

func TestFormMatchByRatingStaysInBand(t *testing.T) {
	h, key := hubWithLobby(
		mkWaitingClient("a", 1000, "", 10), // anchor (longest wait)
		mkWaitingClient("b", 1050, "", 5),  // within band
		mkWaitingClient("c", 1500, "", 1),  // far outside band
	)
	group, _, ok := h.formMatch(key, matchCriteria{GroupSize: 2, MatchBy: "rating", RatingBand: 100})
	if !ok || len(group) != 2 {
		t.Fatalf("expected a 2-peer rating match, got ok=%v group=%v", ok, group)
	}
	if !inGroup(group, "a") || !inGroup(group, "b") || inGroup(group, "c") {
		t.Fatalf("rating match should pair a+b (close), not c (far): %v", group)
	}
}

func TestFormMatchRatingOutOfBandWaits(t *testing.T) {
	h, key := hubWithLobby(
		mkWaitingClient("a", 1000, "", 5),
		mkWaitingClient("b", 1400, "", 3), // 400 apart, band 100, no widening
	)
	if _, _, ok := h.formMatch(key, matchCriteria{GroupSize: 2, MatchBy: "rating", RatingBand: 100}); ok {
		t.Fatal("400-apart peers must NOT match within a fixed band of 100")
	}
}

func TestFormMatchBandWidensWithWait(t *testing.T) {
	h, key := hubWithLobby(
		mkWaitingClient("a", 1000, "", 5), // anchor waited 5s
		mkWaitingClient("b", 1400, "", 3),
	)
	// band = 100 + 100*5 = 600 >= 400, so the wait opens the match.
	if _, _, ok := h.formMatch(key, matchCriteria{GroupSize: 2, MatchBy: "rating", RatingBand: 100, BandWidenPerSec: 100}); !ok {
		t.Fatal("widening band should let a 400-apart pair match after the anchor waited")
	}
}

func TestFormMatchMaxWaitFallback(t *testing.T) {
	h, key := hubWithLobby(
		mkWaitingClient("a", 1000, "", 40), // waited past max_wait
		mkWaitingClient("b", 2000, "", 1),  // 1000 apart
	)
	if _, _, ok := h.formMatch(key, matchCriteria{GroupSize: 2, MatchBy: "rating", RatingBand: 100, MaxWaitSecs: 30}); !ok {
		t.Fatal("a peer past max_wait_secs must match any rating (no starvation)")
	}
}

func TestFormMatchByRegion(t *testing.T) {
	h, key := hubWithLobby(
		mkWaitingClient("a", 0, "eu", 10), // anchor, eu
		mkWaitingClient("b", 0, "us", 5),  // wrong region
		mkWaitingClient("c", 0, "eu", 1),  // same region
	)
	group, _, ok := h.formMatch(key, matchCriteria{GroupSize: 2, MatchBy: "region"})
	if !ok || len(group) != 2 {
		t.Fatalf("expected a same-region match, got ok=%v group=%v", ok, group)
	}
	if !inGroup(group, "a") || !inGroup(group, "c") || inGroup(group, "b") {
		t.Fatalf("region match should pair the two eu peers, not the us one: %v", group)
	}
}

// waitForMatched reports whether a "matched" frame arrives on ch within a
// generous deadline (the ticker fires at matchTickInterval), ignoring any other
// frame. A channel receive is synchronized, so this observes the ticker's work
// without racing on the client's guarded fields.
func waitForMatched(ch chan wsMessage) bool {
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-ch:
			var f map[string]any
			if json.Unmarshal(msg.data, &f) == nil && f["_type"] == "matched" {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestMatchTickerPairsWaitingPeersWithoutNewConnect is the regression guard for
// the "nobody starves" guarantee: two rating-incompatible peers are already
// waiting and NEITHER connects again. Connect-only matching (formMatch called from
// buildModeSession) would strand them forever; the per-lobby ensureMatchTicker
// must re-evaluate on its timer, widen the band past MaxWaitSecs, and pair them.
func TestMatchTickerPairsWaitingPeersWithoutNewConnect(t *testing.T) {
	a := mkWaitingClient("a", 1000, "", 40) // waited past max_wait_secs
	b := mkWaitingClient("b", 2000, "", 40) // 1000 apart - only a widened/expired band pairs them
	a.ch = make(chan wsMessage, 4)
	b.ch = make(chan wsMessage, 4)
	h, key := hubWithLobby(a, b)

	crit := matchCriteria{GroupSize: 2, MatchBy: "rating", RatingBand: 100, MaxWaitSecs: 30}
	h.ensureMatchTicker(key, "room", crit)
	defer func() {
		h.mu.Lock()
		if r := h.rooms[key]; r != nil && r.matchCancel != nil {
			r.matchCancel()
		}
		h.mu.Unlock()
	}()

	if !waitForMatched(a.ch) || !waitForMatched(b.ch) {
		t.Fatal("the match ticker must pair two already-waiting incompatible peers with no further connect")
	}
}

// TestEnsureMatchTickerNoOpWithoutWidening asserts the ticker is not armed when
// time cannot loosen matching (plain matchmaking here): nothing to re-evaluate, so
// no goroutine and no matchCancel is stored.
func TestEnsureMatchTickerNoOpWithoutWidening(t *testing.T) {
	h, key := hubWithLobby(mkWaitingClient("a", 0, "", 1))
	h.ensureMatchTicker(key, "room", matchCriteria{GroupSize: 2}) // no MatchBy, no widening
	h.mu.RLock()
	defer h.mu.RUnlock()
	if r := h.rooms[key]; r == nil || r.matchCancel != nil {
		t.Fatal("plain matchmaking must not arm a re-evaluation ticker")
	}
}

func TestFormMatchClaimsAreExclusive(t *testing.T) {
	h, key := hubWithLobby(
		mkWaitingClient("a", 0, "", 3),
		mkWaitingClient("b", 0, "", 2),
		mkWaitingClient("c", 0, "", 1),
	)
	g1, _, ok1 := h.formMatch(key, matchCriteria{GroupSize: 2})
	if !ok1 {
		t.Fatal("first match should succeed")
	}
	// One peer remains -> a second 2-peer match can't form (claimed peers are excluded).
	if _, _, ok2 := h.formMatch(key, matchCriteria{GroupSize: 2}); ok2 {
		t.Fatalf("second match must not re-pair already-claimed peers (first claimed %v)", g1)
	}
}

// Match room ids must be unguessable: a sequential counter lets a stranger walk into
// m-3, and because it restarts at 1 with the process, a match routed to an authoritative
// endpoint resumes the previous game's persisted state under a reused id.
func TestMatchRoomIDsAreRandom(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		h, key := hubWithLobby(mkWaitingClient("a", 0, "", 2), mkWaitingClient("b", 0, "", 1))
		_, room, ok := h.formMatch(key, matchCriteria{GroupSize: 2})
		if !ok {
			t.Fatal("expected a match")
		}
		if room == "m-1" || len(room) < 12 {
			t.Fatalf("match room %q looks sequential/guessable", room)
		}
		if seen[room] {
			t.Fatalf("match room id %q repeated", room)
		}
		seen[room] = true
	}
}

// A minted match room holds exactly group_size players; anyone else who learns the id
// (a direct ?room= connect, or an in-band switch) joins as a read-only spectator.
func TestMatchRoomIsCappedAtGroupSize(t *testing.T) {
	a, b := mkWaitingClient("a", 0, "", 2), mkWaitingClient("b", 0, "", 1)
	h, lobbyKey := hubWithLobby(a, b)
	_, room, ok := h.formMatch(lobbyKey, matchCriteria{GroupSize: 2})
	if !ok {
		t.Fatal("expected a match")
	}
	matchKey := matchRoomKey(lobbyKey, room)
	if _, ok := h.moveClient(a, lobbyKey, "lobby", matchKey, room, 0); !ok {
		t.Fatal("move a")
	}
	if _, ok := h.moveClient(b, lobbyKey, "lobby", matchKey, room, 0); !ok {
		t.Fatal("move b")
	}
	if a.spectator || b.spectator {
		t.Fatal("the matched peers themselves must be players")
	}
	intruder := h.join(matchKey, room, "x", 0, nil, nil) // endpoint max_clients is 0 on a matchmaking endpoint
	if !intruder.spectator {
		t.Fatal("a third peer joining a full match room was admitted as a player - match rooms must cap at group_size")
	}
}
