package api

import (
	"fmt"
	"strings"

	"openfabrica/internal/wsendpoint"
	"openfabrica/tools"
)

// manage_matchmaking is the MATCHMAKING realtime model: peers connect to a lobby
// and wait; once group_size unmatched peers are present the server mints ONE
// shared match room, assigns roles, and pushes each a {_type:'matched',...}
// frame the ofnet helper acts on. It replaces hand-rolled client-side pairing
// (the source of "both peers land in different rooms" and deadlock bugs). Cannot
// share an endpoint with an authoritative reducer or room_gated entry. Shares
// the of_ws_endpoints runtime.
//
// By default it pairs ANY group_size waiting peers. Set match_by (rating/region)
// to pair by skill and/or region; peers pass {rating, region} in their ?meta=
// presence profile, and a waiting peer's rating band widens with time when
// band_widen_per_sec / max_wait_secs are set (both default to 0 = fixed band). See formMatch (server/public/ws_hub.go).

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_matchmaking",
		RealtimeModel:      "matchmaking",
		Category:           "api",
		FullResultInEvents: true,
		MaxResult:          16000,
		OneLiner:           "Multiplayer matchmaking lobby (WebSocket): the server auto-pairs waiting peers into a shared room (optionally by skill/region).",
		WhenToUse: `Use for "find me an opponent" / auto-match. Each peer connects to the
lobby and waits; once group_size unmatched peers are present the SERVER mints
one shared match room, assigns roles, and switches them in. Do NOT hand-roll
pairing in client JS (peers land in different rooms / deadlock). For skill/region-based
pairing set match_by and have peers carry {rating, region} in their meta (the
rating band is FIXED unless you also set band_widen_per_sec and max_wait_secs,
so for match_by=rating set both, e.g. 50 and 30, or an outlier waits forever). For an
auto-matched AUTHORITATIVE game, pair here then connect the matched peers to a
separate manage_authoritative endpoint via ?room= (matchmaking and a reducer
cannot share one endpoint). Read/delete with manage_realtime.`,
		Actions: []tools.Action{
			&MatchmakingCreate{},
			&MatchmakingUpdate{},
		},
		Summarize: EpSummarize,
	})
}

// validMatchBy is the closed set of matchmaking criteria. "" = pair any peers.
var validMatchBy = map[string]bool{"": true, "rating": true, "region": true, "rating+region": true}

// matchmakingOpts carries the criteria-based tuning columns. Every field is
// tri-state (nil = "not provided") because each one documents a meaningful zero -
// "" pairs any peer, 0 fixes the band, 0 never forces a match - which a plain
// value could not express against a "only write what is set" patch.
type matchmakingOpts struct {
	MatchBy    *string
	RatingBand *int
	BandWiden  *float64
	MaxWait    *int
}

// epSetMatchmakingOpts patches the criteria-based matchmaking tuning columns on
// an existing ws endpoint row. Only PROVIDED values are written (so it is safe
// from both create - after the base row exists - and update), and match_by is
// validated. An all-unset call is a no-op success.
func epSetMatchmakingOpts(ctx *tools.ToolContext, rawPath string, opts matchmakingOpts) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(rawPath)
	sets := []string{}
	vals := []any{}
	matchBy := ""
	if opts.MatchBy != nil {
		if !validMatchBy[*opts.MatchBy] {
			return tools.Fail(`match_by must be one of: "" (any peer), "rating", "region", "rating+region"`), nil
		}
		matchBy = *opts.MatchBy
		sets = append(sets, "match_by = ?")
		vals = append(vals, matchBy)
	}
	if opts.RatingBand != nil {
		if *opts.RatingBand < 0 {
			return tools.Fail("rating_band must be >= 0 (got %d)", *opts.RatingBand), nil
		}
		sets = append(sets, "rating_band = ?")
		vals = append(vals, *opts.RatingBand)
	}
	if opts.BandWiden != nil {
		if *opts.BandWiden < 0 {
			return tools.Fail("band_widen_per_sec must be >= 0 (got %v)", *opts.BandWiden), nil
		}
		sets = append(sets, "band_widen_per_sec = ?")
		vals = append(vals, *opts.BandWiden)
	}
	if opts.MaxWait != nil {
		if *opts.MaxWait < 0 {
			return tools.Fail("max_wait_secs must be >= 0 (got %d)", *opts.MaxWait), nil
		}
		sets = append(sets, "max_wait_secs = ?")
		vals = append(vals, *opts.MaxWait)
	}
	if len(sets) == 0 {
		return tools.Ok(map[string]any{"endpoint_path": path}), nil
	}
	vals = append(vals, path)
	if _, err := ctx.DB.Exec("UPDATE of_ws_endpoints SET "+strings.Join(sets, ", ")+" WHERE path = ?", vals...); err != nil {
		return nil, fmt.Errorf("setting matchmaking options: %w", err)
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "match_by": matchBy}), nil
}

// --- create -----------------------------------------------------------------

type MatchmakingCreate struct {
	EndpointPath    string  `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash, no :param)."`
	GroupSize       int     `arg:"group_size"             desc:"Peers per match (2-16). Default 2." default:"2"`
	RequiresAuth    bool    `arg:"requires_auth"          desc:"Connections must include a valid token (connect() auto-carries it)."`
	RoomColumn      string  `arg:"room_column"            desc:"Field matched peers switch on (defaults to 'room'). The ofnet helper reads room_field from the matched frame automatically."`
	MaxMsgSize      int     `arg:"max_msg_size"           desc:"Hard cap on single inbound frame bytes, max 1048576 (1 MiB)." default:"16384"`
	MatchBy         string  `arg:"match_by"               desc:"Criteria-based pairing: 'rating' (close skill), 'region' (same region), or 'rating+region'. Omit to pair any group_size peers. Peers pass {rating, region} in their meta: connect(path, {meta:{rating:1200, region:'eu'}})."`
	RatingBand      int     `arg:"rating_band"            desc:"match_by=rating or rating+region: initial rating half-width a match may span (default 200)."`
	BandWidenPerSec float64 `arg:"band_widen_per_sec"     desc:"match_by=rating or rating+region: how much the band grows per second the longest-waiting peer has waited (e.g. 50). 0 = fixed band."`
	MaxWaitSecs     int     `arg:"max_wait_secs"          desc:"match_by=rating or rating+region: after waiting this many seconds a peer matches ANY rating (region still honored) so nobody starves. 0 (the default) = never force, so set it when you set match_by."`
}

func (a *MatchmakingCreate) Doc() string {
	return "Create a matchmaking lobby endpoint (server pairs peers into a shared match room; optionally by rating/region)."
}
func (a *MatchmakingCreate) Example() any {
	return &MatchmakingCreate{EndpointPath: "race", GroupSize: 2}
}
func (a *MatchmakingCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	// Validate before the endpoint row exists: create is a no-op on an existing path,
	// so a bad match_by rejected after it would leave a half-configured endpoint that
	// the corrected retry can no longer fix with create.
	if !validMatchBy[a.MatchBy] {
		return tools.Fail(`match_by must be one of: "" (any peer), "rating", "region", "rating+region"`), nil
	}
	base, err := epCreateWebSocket(ctx, wsEndpointConfig{
		Model:        wsendpoint.ModelMatchmaking,
		Path:         a.EndpointPath,
		RequiresAuth: &a.RequiresAuth,
		Matchmaking:  true,
		GroupSize:    a.GroupSize,
		RoomColumn:   a.RoomColumn,
		MaxMsgSize:   a.MaxMsgSize,
	})
	if err != nil || (base != nil && !base.Success) {
		return base, err
	}
	if m, ok := base.Data.(map[string]any); ok && m["already_existed"] == true {
		return base, nil // create on an existing path applies nothing, matchmaking options included
	}
	// Create writes only what the caller actually set, so an unmentioned column
	// keeps its schema default (rating_band 200) rather than being zeroed.
	opts := matchmakingOpts{}
	if a.MatchBy != "" {
		opts.MatchBy = &a.MatchBy
	}
	band := a.RatingBand
	if a.MatchBy != "" && band <= 0 {
		band = 200
	}
	if band > 0 {
		opts.RatingBand = &band
	}
	if a.BandWidenPerSec > 0 {
		opts.BandWiden = &a.BandWidenPerSec
	}
	if a.MaxWaitSecs > 0 {
		opts.MaxWait = &a.MaxWaitSecs
	}
	if r2, e2 := epSetMatchmakingOpts(ctx, a.EndpointPath, opts); e2 != nil || (r2 != nil && !r2.Success) {
		return r2, e2
	}
	return base, nil
}

// --- update -----------------------------------------------------------------

type MatchmakingUpdate struct {
	EndpointPath    string   `arg:"endpoint_path,required" desc:"Matchmaking endpoint path to update."`
	GroupSize       int      `arg:"group_size"             desc:"Replace peers-per-match (2-16)."`
	RequiresAuth    *bool    `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	RoomColumn      string   `arg:"room_column"            desc:"Replace the room-switch field (defaults to 'room')."`
	MaxMsgSize      int      `arg:"max_msg_size"           desc:"Replace single-frame byte cap, max 1048576 (1 MiB)."`
	MatchBy         *string  `arg:"match_by"               desc:"Replace pairing criteria: '' (pair any peer, turning criteria pairing off) | 'rating' | 'region' | 'rating+region'. Omit to leave unchanged."`
	RatingBand      *int     `arg:"rating_band"            desc:"Replace the initial rating half-width (match_by=rating). Omit to leave unchanged."`
	BandWidenPerSec *float64 `arg:"band_widen_per_sec"     desc:"Replace the per-second band widening (match_by=rating); 0 fixes the band. Omit to leave unchanged."`
	MaxWaitSecs     *int     `arg:"max_wait_secs"          desc:"Replace the wait after which a peer matches any rating (match_by=rating); 0 never forces a match. Omit to leave unchanged."`
}

func (a *MatchmakingUpdate) Doc() string {
	return "Update a matchmaking endpoint's options (group size, auth, and skill/region pairing)."
}
func (a *MatchmakingUpdate) Example() any {
	return &MatchmakingUpdate{EndpointPath: "race", GroupSize: 4}
}
func (a *MatchmakingUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	base, err := epUpdateWebSocket(ctx, wsEndpointConfig{
		Model:        wsendpoint.ModelMatchmaking,
		Path:         a.EndpointPath,
		RequiresAuth: a.RequiresAuth,
		Matchmaking:  true,
		GroupSize:    a.GroupSize,
		RoomColumn:   a.RoomColumn,
		MaxMsgSize:   a.MaxMsgSize,
	})
	if err != nil || (base != nil && !base.Success) {
		return base, err
	}
	if r2, e2 := epSetMatchmakingOpts(ctx, a.EndpointPath, matchmakingOpts{
		MatchBy:    a.MatchBy,
		RatingBand: a.RatingBand,
		BandWiden:  a.BandWidenPerSec,
		MaxWait:    a.MaxWaitSecs,
	}); e2 != nil || (r2 != nil && !r2.Success) {
		return r2, e2
	}
	return base, nil
}
