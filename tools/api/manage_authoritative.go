package api

import (
	"openfabrica/internal/wsendpoint"
	"openfabrica/tools"
)

// manage_authoritative is the AUTHORITATIVE realtime model: a server-side
// reducer (goja, sandboxed) owns the room's state, validates every action, and
// pushes each client its view(). It is the right model whenever the SERVER must
// own contested, cheat-sensitive, or hidden-information shared state - games are
// the most common case (turn-based, scored, hidden hands), but the same model
// fits auctions/bidding, seat or inventory booking under contention, live
// voting/quizzes, and any collaborative state that needs server arbitration.
// Create the endpoint, then set_reducer to attach reduce(state,event,ctx).
// Cannot be combined with matchmaking on one endpoint (pair on a
// manage_matchmaking endpoint, then route matched peers here via ?room=). Shares
// the of_ws_endpoints runtime.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_authoritative",
		RealtimeModel:      "authoritative",
		Category:           "api",
		FullResultInEvents: true,
		MaxResult:          16000,
		OneLiner:           "Authoritative multiplayer WebSocket: a server reducer owns & validates shared state (turn-based games, auctions; anti-cheat).",
		WhenToUse: `Use whenever the SERVER must own the truth for contested, cheat-sensitive, or
hidden-information shared state. Games are the most common case (turn-based,
scored, hidden hands), but the same model fits auctions/bidding, seat or
inventory booking under contention, live voting/quizzes, and collaborative state
that needs server arbitration. Two steps: create the endpoint, then set_reducer
with reduce(state, event, ctx) (optional view(state, playerId) to hide info per
player). The reducer runs sandboxed in a per-room actor and persists durable
state itself via ctx.insert/update/upsert - never trust a client POST for
money/score/result. Read the current reducer with action=get before fixing it. Cannot also be
a matchmaking endpoint: pair on manage_matchmaking, then connect matched peers
here via ?room=. See the realtime_signaling guide.`,
		Actions: []tools.Action{
			&AuthoritativeCreate{},
			&AuthoritativeUpdate{},
			&AuthoritativeSetReducer{},
			&AuthoritativeGet{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type AuthoritativeCreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash, no :param). Attach the reducer with set_reducer after creating."`
	RequiresAuth bool   `arg:"requires_auth"          desc:"Connections must include a valid token. Required for durable per-user settlement: player.uid (the server-stamped authed id) is present only on a requires_auth endpoint - absent otherwise. connect() auto-carries the token. For an ANONYMOUS presence game (requires_auth off) key your in-memory roster on event.player.id (the always-present client id), NOT player.uid."`
	RoomColumn   string `arg:"room_column"            desc:"Column name used for room scoping (each room is an independent authoritative session - a game table, an auction, a booking room)."`
	TickHz       int    `arg:"tick_hz"                desc:"Optional: for a CONTINUOUSLY-SIMULATED authoritative game, the actor delivers a {type:'@tick',dt} event this many times/sec (up to 120) so reduce() advances the sim server-side (heaviest mode; small player counts). Omit for turn-based." max:"120"`
	MaxClients   int    `arg:"max_clients"            desc:"Cap on participants per room (0 = unlimited). Overflow joiners connect as read-only spectators."`
	Ephemeral    bool   `arg:"ephemeral"              desc:"Throwaway rooms: when a room empties, drop its persisted reducer state so the next game in that room starts from @init. Set it for per-match rooms (a matchmaking hand-off), or their state is kept forever and a reused room id resumes an old game."`
}

func (a *AuthoritativeCreate) Doc() string {
	return "Create an authoritative endpoint (server-owned state - games, auctions, bookings, arbitrated collaboration). Follow with set_reducer to attach the server-side reducer."
}
func (a *AuthoritativeCreate) Example() any {
	return &AuthoritativeCreate{EndpointPath: "game", RequiresAuth: true}
}
func (a *AuthoritativeCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateWebSocket(ctx, wsEndpointConfig{
		Model:         wsendpoint.ModelAuthoritative,
		Path:          a.EndpointPath,
		RequiresAuth:  &a.RequiresAuth,
		RoomColumn:    a.RoomColumn,
		TickHz:        a.TickHz,
		MaxClients:    &a.MaxClients,
		Ephemeral:     a.Ephemeral,
		Authoritative: true,
	})
}

// --- update -----------------------------------------------------------------

type AuthoritativeUpdate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Authoritative endpoint path to update."`
	RequiresAuth *bool  `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	RoomColumn   string `arg:"room_column"            desc:"Replace the room-scope column."`
	TickHz       int    `arg:"tick_hz"                desc:"Replace the server-sim tick rate (see create; up to 120)." max:"120"`
	MaxClients   *int   `arg:"max_clients"            desc:"Replace the per-room participant cap; 0 lifts the cap (unlimited). Omit to leave unchanged."`
	Ephemeral    bool   `arg:"ephemeral"              desc:"Turn on dropping a room's persisted reducer state when it empties. Only flips on."`
}

func (a *AuthoritativeUpdate) Doc() string {
	return "Update an authoritative endpoint's options (not its reducer - use set_reducer)."
}
func (a *AuthoritativeUpdate) Example() any {
	four := 4
	return &AuthoritativeUpdate{EndpointPath: "game", MaxClients: &four}
}
func (a *AuthoritativeUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateWebSocket(ctx, wsEndpointConfig{
		Model:        wsendpoint.ModelAuthoritative,
		Path:         a.EndpointPath,
		RequiresAuth: a.RequiresAuth,
		RoomColumn:   a.RoomColumn,
		TickHz:       a.TickHz,
		MaxClients:   a.MaxClients,
		Ephemeral:    a.Ephemeral,
	})
}

// --- set_reducer ------------------------------------------------------------

type AuthoritativeSetReducer struct {
	EndpointPath  string   `arg:"endpoint_path,required" desc:"Authoritative endpoint to attach/replace the server-side reducer on."`
	Code          string   `arg:"code,required"          desc:"Server-side reducer JS defining reduce(state, event, ctx), and optionally view(state, playerId) for hidden info. Runs sandboxed in a per-room actor; never served to the browser. The rules it must satisfy are the room-reducer contract, carried in this endpoint's create result and in the realtime_signaling guide: they are NOT guessable from the JavaScript (Math.random() and Date.now() throw, a throw freezes the room, ctx.after fires once, @tick dt is seconds). Read them before writing this, and use action=get to read the current reducer before fixing it."`
	SeedTable     string   `arg:"seed_table"       desc:"Optional: table the reducer's authoritative state is seeded from at @join. The actor reads seed_columns WHERE seed_key_column = <authenticated user id> and injects them as event.player.seed, so balances come from the DB, not client meta. Requires requires_auth."`
	SeedKeyColumn string   `arg:"seed_key_column"  desc:"Column in seed_table matched against the joining user's authenticated id. Defaults to 'id'."`
	SeedColumns   []string `arg:"seed_columns"     desc:"Columns to read from seed_table into event.player.seed (e.g. [\"bankroll\",\"role\"])."`
	SharedTable   string   `arg:"shared_table"     desc:"Optional CROSS-ROOM read: a (global, not per-player) table whose rows are injected as event.shared on every non-@tick event, so the reducer can read state shared across rooms mid-reduce - a progressive jackpot pooled across game rooms, a global config/limits row, a leaderboard's top rows. Authority stays per-room; rows are capped. Unlike seed_table (keyed on the joining user), this is the whole (small) table."`
	SharedColumns []string `arg:"shared_columns"   desc:"Columns to read from shared_table into event.shared (e.g. [\"pool\",\"max_bet\"]). Omit to read all NON-SECURE columns (PASSWORD/ENCRYPTED columns are always excluded from event.shared)."`
}

func (a *AuthoritativeSetReducer) Doc() string {
	return "Attach/replace the authoritative room reducer. Validated at save time. Optionally declare a DB seed (seed_table/seed_columns) so the reducer seeds durable state from the database at join."
}
func (a *AuthoritativeSetReducer) Example() any {
	return &AuthoritativeSetReducer{EndpointPath: "game", Code: "function reduce(state, event, ctx) {\n  if (!state) state = { players: {} };\n  var p = event.player;                 // p.id === the client's net.myId; ALWAYS present (even anonymous)\n  if (event.type === '@join')  { state.players[p.id] = { id: p.id, name: (p.meta && p.meta.name) || 'Guest', x: 0, y: 0 }; return state; }\n  if (event.type === '@leave') { delete state.players[p.id]; return state; }\n  if (event.type === 'MOVE')   { var me = state.players[p.id]; if (me) { me.x = event.x; me.y = event.y; } return state; }   // net.action('MOVE', {x,y}) spreads the payload at the top level\n  return state;\n}\nfunction view(state, playerId) { return state; }   // all players share one roster; key on player.id, never player.uid"}
}
func (a *AuthoritativeSetReducer) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epSetWebSocketReducer(ctx, a.EndpointPath, a.Code, a.SeedTable, a.SeedKeyColumn, a.SeedColumns, a.SharedTable, a.SharedColumns)
}

// --- get --------------------------------------------------------------------

type AuthoritativeGet struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Authoritative endpoint to fetch the full config for, INCLUDING the reducer source. Read the current reducer here, fix the exact line, then re-attach with set_reducer (don't blind-rewrite it)."`
}

func (a *AuthoritativeGet) Doc() string {
	return "Fetch one endpoint's full config plus its current reducer source. Use this before fixing a reducer."
}
func (a *AuthoritativeGet) ReadOnly() bool { return true }
func (a *AuthoritativeGet) Example() any   { return &AuthoritativeGet{EndpointPath: "game"} }
func (a *AuthoritativeGet) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epGetWebSocket(ctx, a.EndpointPath)
}
