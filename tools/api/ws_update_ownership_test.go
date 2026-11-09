package api

// Each realtime model tool owns its own endpoints. Before the stored model, any tool's
// update could be aimed at another model's endpoint and silently convert or break it,
// and two updates corrupted config on every call:
//
//   - manage_matchmaking update reset a custom room_column back to "room";
//   - manage_signaling update rewrote tick_hz=0 and max_msg_size=64KB.
//
// set_reducer also accepted a seed that could never fire and a reducer that throws on
// its first @join, reporting both only at build end (or never).

import (
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/internal/wsendpoint"
	"openfabrica/tools"
)

func newWSToolCtx(t *testing.T) *tools.ToolContext {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	return &tools.ToolContext{DB: pdb.Writer(), ProjectID: 1}
}

type runner interface {
	Run(*tools.ToolContext) (*tools.Result, error)
}

func mustOK(t *testing.T, ctx *tools.ToolContext, a runner) {
	t.Helper()
	r, err := a.Run(ctx)
	if err != nil || r == nil || !r.Success {
		t.Fatalf("%T failed: err=%v res=%+v", a, err, r)
	}
}

func mustFail(t *testing.T, ctx *tools.ToolContext, a runner, want string) {
	t.Helper()
	r, err := a.Run(ctx)
	if err != nil {
		t.Fatalf("%T: unexpected error %v", a, err)
	}
	if r == nil || r.Success {
		t.Fatalf("%T succeeded, want a refusal containing %q", a, want)
	}
	if msg := r.Error; !strings.Contains(msg, want) {
		t.Fatalf("%T refusal = %q, want it to mention %q", a, msg, want)
	}
}

func loadWS(t *testing.T, ctx *tools.ToolContext, path string) *wsendpoint.Row {
	t.Helper()
	row, err := wsendpoint.Load(ctx.DB, path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return row
}

func TestRealtimeCreateStoresModel(t *testing.T) {
	ctx := newWSToolCtx(t)
	mustOK(t, ctx, &RelayCreate{EndpointPath: "chat"})
	mustOK(t, ctx, &TickCreate{EndpointPath: "arena", TickHz: 30})
	mustOK(t, ctx, &AuthoritativeCreate{EndpointPath: "game"})
	mustOK(t, ctx, &MatchmakingCreate{EndpointPath: "lobby", GroupSize: 2})
	mustOK(t, ctx, &SignalingCreate{EndpointPath: "call"})
	for path, want := range map[string]string{
		"chat": wsendpoint.ModelRelay, "arena": wsendpoint.ModelTick, "game": wsendpoint.ModelAuthoritative,
		"lobby": wsendpoint.ModelMatchmaking, "call": wsendpoint.ModelSignaling,
	} {
		if got := loadWS(t, ctx, path).Model; got != want {
			t.Errorf("%s stored model %q, want %q", path, got, want)
		}
	}
}

func TestRealtimeUpdateRefusesAnotherModelsEndpoint(t *testing.T) {
	ctx := newWSToolCtx(t)
	mustOK(t, ctx, &TickCreate{EndpointPath: "arena", TickHz: 30})
	mustOK(t, ctx, &MatchmakingCreate{EndpointPath: "lobby"})

	two := 2
	mustFail(t, ctx, &SignalingUpdate{EndpointPath: "arena", MaxClients: &two}, "manage_tick")
	if row := loadWS(t, ctx, "arena"); row.TickHz != 30 {
		t.Fatalf("a refused signaling update still changed tick_hz to %d", row.TickHz)
	}
	// room_gated on a matchmaking lobby is a combo the runtime cannot serve.
	mustFail(t, ctx, &RelayUpdate{EndpointPath: "lobby", RoomGated: true}, "manage_matchmaking")
	if loadWS(t, ctx, "lobby").RoomGated {
		t.Fatal("room_gated was turned on for a matchmaking lobby")
	}
}

func TestMatchmakingUpdateKeepsCustomRoomColumn(t *testing.T) {
	ctx := newWSToolCtx(t)
	mustOK(t, ctx, &MatchmakingCreate{EndpointPath: "race", RoomColumn: "match"})
	mustOK(t, ctx, &MatchmakingUpdate{EndpointPath: "race", GroupSize: 4})
	row := loadWS(t, ctx, "race")
	if row.RoomColumn != "match" {
		t.Fatalf("room_column = %q after a group_size update, want the custom \"match\"", row.RoomColumn)
	}
	if row.GroupSize != 4 {
		t.Fatalf("group_size = %d, want 4", row.GroupSize)
	}
}

func TestSignalingUpdateKeepsFrameCap(t *testing.T) {
	ctx := newWSToolCtx(t)
	mustOK(t, ctx, &SignalingCreate{EndpointPath: "call", MaxMsgSize: 131072})
	two := 2
	mustOK(t, ctx, &SignalingUpdate{EndpointPath: "call", MaxClients: &two})
	row := loadWS(t, ctx, "call")
	if row.MaxMsgSize != 131072 {
		t.Fatalf("max_msg_size = %d after a max_clients update, want the custom 131072", row.MaxMsgSize)
	}
	if row.MaxClients != 2 {
		t.Fatalf("max_clients = %d, want 2", row.MaxClients)
	}
}

func TestWriteToTableMustExist(t *testing.T) {
	ctx := newWSToolCtx(t)
	mustFail(t, ctx, &RelayCreate{EndpointPath: "chat", WriteToTable: "messages"}, "does not exist")
	mustFail(t, ctx, &RelayCreate{EndpointPath: "chat2", WriteToTable: "of_users"}, "write_to_table rejected")
	mustFail(t, ctx, &RelayCreate{EndpointPath: "chat3", RoomColumn: "bad-name"}, "invalid room_column")
}

func TestSetReducerChecks(t *testing.T) {
	ctx := newWSToolCtx(t)
	mustOK(t, ctx, &RelayCreate{EndpointPath: "chat"})
	mustOK(t, ctx, &AuthoritativeCreate{EndpointPath: "game"})
	const ok = `function reduce(state, event, ctx) { if (event.type === '@init') return { n: 0 }; return state; }`

	// Only a manage_authoritative endpoint takes a reducer.
	mustFail(t, ctx, &AuthoritativeSetReducer{EndpointPath: "chat", Code: ok}, "manage_relay")

	// A seed keyed on the authenticated id can never fire on an anonymous endpoint.
	mustFail(t, ctx, &AuthoritativeSetReducer{EndpointPath: "game", Code: ok, SeedTable: "players", SeedColumns: []string{"bankroll"}}, "requires_auth")

	// A reducer that throws on @join is rejected at attach time, not at build end.
	const throws = `function reduce(state, event, ctx) {
  if (event.type === '@init') return { players: {} };
  if (event.type === '@join') { state.players[event.player.id].score++; return state; }
  return state;
}`
	mustFail(t, ctx, &AuthoritativeSetReducer{EndpointPath: "game", Code: throws}, "NOT attached")
	if loadWS(t, ctx, "game").Reducer != "" {
		t.Fatal("a reducer that failed the lifecycle run was stored anyway")
	}

	mustOK(t, ctx, &AuthoritativeSetReducer{EndpointPath: "game", Code: ok})
	if row := loadWS(t, ctx, "game"); !row.Authoritative || row.Reducer == "" {
		t.Fatalf("valid reducer not attached: authoritative=%v", row.Authoritative)
	}
}

// A bad match_by must be refused BEFORE the endpoint row exists: create is a no-op on
// an existing path, so a late rejection left a half-made lobby the corrected create
// call could not fix.
func TestMatchmakingCreateRejectsBadMatchByWithoutCreating(t *testing.T) {
	ctx := newWSToolCtx(t)
	mustFail(t, ctx, &MatchmakingCreate{EndpointPath: "race", MatchBy: "skill"}, "match_by")
	mustOK(t, ctx, &MatchmakingCreate{EndpointPath: "race", MatchBy: "rating"})
	row := loadWS(t, ctx, "race")
	if row.Model != wsendpoint.ModelMatchmaking || row.MatchBy != "rating" {
		t.Fatalf("corrected create did not configure the lobby (model %q, match_by %q)", row.Model, row.MatchBy)
	}
}
