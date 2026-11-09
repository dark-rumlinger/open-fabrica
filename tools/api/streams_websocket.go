package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coder/websocket"

	"openfabrica/internal/endpoints"
	"openfabrica/internal/wsendpoint"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

// WebSocket transport handlers shared by the per-model realtime tools. Tool
// registration and arg shape live in manage_relay/tick/authoritative/matchmaking/
// signaling.go (list/get/delete in manage_realtime.go); all build a wsEndpointConfig.

// errSignalingTick is returned when a caller sets mode=signaling with tick_hz>0.
// Signaling is an immediate WebRTC relay and cannot carry a server tick, so rather
// than silently zeroing tick_hz (turning a tick endpoint into a relay) we reject it.
const errSignalingTick = "a manage_signaling endpoint is an immediate WebRTC relay and cannot carry a server tick (tick_hz is fixed at 0). For tick-rate snapshots (positions/cursors) create a manage_tick endpoint with tick_hz 20/30/60 (1-120 accepted)."

// validateRealtimeCombos backstops the mutually-exclusive realtime-model combinations
// that share the of_ws_endpoints union but cannot coexist at runtime. The per-model
// tools already make these unrepresentable in their arg structs, so this fires only
// for the shared create/update handlers. Returns "" when legal. Combos that depend on
// the STORED row are checked separately against the stored row (checkModelOwnership).
func validateRealtimeCombos(signaling bool, tickHz int, matchmaking, roomGated bool) string {
	if signaling && tickHz != 0 {
		return errSignalingTick
	}
	if matchmaking && roomGated {
		return "matchmaking is incompatible with room_gated (matched peers switch rooms in-band, which room_gated disables)"
	}
	return ""
}

// WebSocket endpoint actions

// modelTool names the tool that owns a realtime model, for refusal messages.
// A model has two names: the stored of_ws_endpoints.model value
// (relay/tick/authoritative/matchmaking/signaling) and the plan-choice/trait name
// "webrtc" for the signaling model. Map both to the real tool here so a refusal can
// never print a non-existent "manage_webrtc" (an explicit map, never "manage_"+model).
func modelTool(model string) string {
	switch model {
	case wsendpoint.ModelRelay:
		return "manage_relay"
	case wsendpoint.ModelTick:
		return "manage_tick"
	case wsendpoint.ModelAuthoritative:
		return "manage_authoritative"
	case wsendpoint.ModelMatchmaking:
		return "manage_matchmaking"
	case wsendpoint.ModelSignaling, "webrtc":
		return "manage_signaling"
	default:
		return "a realtime tool"
	}
}

// checkModelOwnership refuses an update/set_reducer aimed at an endpoint another realtime
// model owns. Each model tool's update exposes only its own model's options, so letting it
// patch a foreign row silently converts it (manage_signaling zeroing a tick endpoint's
// tick_hz) or builds a combo the runtime cannot serve (room_gated on a matchmaking lobby).
// want "" skips the check.
func checkModelOwnership(row *wsendpoint.Row, want string) *tools.Result {
	if want == "" || row == nil {
		return nil
	}
	have := row.EffectiveModel()
	if have == want {
		return nil
	}
	// A legacy row (no stored model) cannot tell signaling from relay; let the two
	// siblings through rather than refuse an endpoint that may well be theirs.
	if row.Model == "" && have == wsendpoint.ModelRelay && want == wsendpoint.ModelSignaling {
		return nil
	}
	return tools.Fail("endpoint %q is a %s endpoint (created with %s); update it with %s, not %s. To change its model, delete it with manage_realtime and create it with the other tool.",
		row.Path, have, modelTool(have), modelTool(have), modelTool(want))
}

// validateWSPersistTarget checks write_to_table / room_column before they are stored.
// The relay interpolates both into SQL at runtime and silently degrades when they are
// wrong (a missing table falls back to a raw `data` insert that fails per frame), so a
// bad name must fail here, at author time, where the model can fix it.
func validateWSPersistTarget(ctx *tools.ToolContext, writeToTable, roomColumn string) *tools.Result {
	if roomColumn != "" && !store.ValidColumnName.MatchString(roomColumn) {
		return tools.Fail("invalid room_column name: %s", roomColumn)
	}
	if writeToTable == "" {
		return nil
	}
	if _, err := store.SanitizedTableName(writeToTable); err != nil {
		return tools.Fail("%s", "write_to_table rejected: "+err.Error())
	}
	cols, exists := dynamicTableColumns(ctx, writeToTable)
	if !exists {
		return tools.Fail("write_to_table %q does not exist - create it first (manage_schema create_table), then set write_to_table", writeToTable)
	}
	if roomColumn != "" && cols != nil && !cols[roomColumn] {
		return tools.Fail("room_column %q is not a column of write_to_table %q: the relay stamps each persisted row's room into it (and ephemeral cleanup deletes by it), so add the column or pick an existing one", roomColumn, writeToTable)
	}
	return nil
}

// dynamicTableColumns returns a project table's column set (nil when its schema is
// unreadable) and whether the table exists at all.
func dynamicTableColumns(ctx *tools.ToolContext, table string) (map[string]bool, bool) {
	var def sql.NullString
	if err := ctx.DB.QueryRow("SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?", table).Scan(&def); err != nil {
		return nil, false
	}
	if !def.Valid || def.String == "" {
		return nil, true
	}
	return store.SchemaDefColumnSet(def.String), true
}

// epCreateWebSocket creates an endpoint from the per-model tools' shared config. Zero-valued
// optional ints (tickHz/wsInboundRate/maxMsgSize) mean "unset" and use per-mode defaults.
func epCreateWebSocket(ctx *tools.ToolContext, cfg wsEndpointConfig) (*tools.Result, error) {
	requiresAuth := cfg.RequiresAuth != nil && *cfg.RequiresAuth // create always passes a non-nil ptr (default false)
	writeToTableArg := cfg.WriteToTable
	roomColumn := cfg.RoomColumn
	tenantColumnArg := cfg.TenantColumn
	mode := cfg.Mode
	tickHzArg := cfg.TickHz
	wsInboundRate := cfg.WSInboundRate
	maxMsgSizeArg := cfg.MaxMsgSize
	e2ee := cfg.E2EE
	ephemeralArg := cfg.Ephemeral
	roomGatedArg := cfg.RoomGated
	matchmaking := cfg.Matchmaking
	groupSizeArg := cfg.GroupSize
	maxClientsArg := 0
	if cfg.MaxClients != nil {
		maxClientsArg = *cfg.MaxClients
	}
	messageTTLArg := cfg.MessageTTL
	authoritative := cfg.Authoritative

	path, errRes := tools.ValidateAndNormalizeEndpointPath(cfg.Path)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	// Skip if WebSocket endpoint already exists (prevents duplicate creation during BUILD).
	var existingCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_ws_endpoints WHERE path = ?", path).Scan(&existingCount); err == nil && existingCount > 0 {
		// Still (re-)emit the client helper: ArtifactSave upserts, and a helper emit that
		// failed on the original create would otherwise never heal (this branch returns early).
		emitRealtimeClientHelper(ctx.DB)
		// Same for the crypto helper on an e2ee endpoint. Helper emits are best-effort,
		// so a create whose e2ee emit failed leaves an endpoint that REJECTS plaintext
		// with no js/e2ee.js to encrypt with - every page importing 'e2ee' then fails
		// module resolution and the room is dead until someone runs an update.
		var existingE2EE int
		if err := ctx.DB.QueryRow("SELECT COALESCE(e2ee, 0) FROM of_ws_endpoints WHERE path = ?", path).Scan(&existingE2EE); err == nil && existingE2EE == 1 {
			emitE2EEClientHelper(ctx.DB)
		}
		// Same for the spine claim: ClaimEndpoint is idempotent, and a claim that failed on
		// the original create would otherwise never heal, leaving /api/<path>/ws unroutable.
		if err := ClaimEndpoint(ctx, endpoints.KindWS, path, nil); err != nil {
			return tools.FailErr(err), nil
		}
		return tools.Ok(map[string]any{
			"endpoint_path":   path,
			"already_existed": true,
			"message":         "WebSocket endpoint /api/" + path + " already exists, so nothing was created and NO option in this call was applied. Change its config with the update action of the tool that owns this model (manage_relay / manage_tick / manage_authoritative / manage_matchmaking / manage_signaling).",
		}), nil
	}

	writeToTable := ""
	if writeToTableArg != "" {
		if !store.ValidColumnName.MatchString(writeToTableArg) {
			return tools.Fail("invalid write_to_table name: %s", writeToTableArg), nil
		}
		writeToTable = writeToTableArg
	}

	if res := validateWSPersistTarget(ctx, writeToTable, roomColumn); res != nil {
		return res, nil
	}

	tenantColumn := ""
	if tenantColumnArg != "" {
		if !store.ValidColumnName.MatchString(tenantColumnArg) {
			return tools.Fail("invalid tenant_column name: %s", tenantColumnArg), nil
		}
		tenantColumn = tenantColumnArg
	}

	if tenantColumn != "" && !requiresAuth {
		return tools.Fail("tenant_column requires requires_auth=true (otherwise tenant scoping has no trusted identity to filter on)"), nil
	}

	if mode != "" && mode != "chat" && mode != "signaling" {
		return tools.Fail("mode must be 'chat' or 'signaling' (got %q)", mode), nil
	}
	signaling := mode == "signaling"

	tickHz := tickHzArg
	if tickHz < 0 || tickHz > maxTickHz {
		return tools.Fail("tick_hz must be between 0 and %d (got %d)", maxTickHz, tickHz), nil
	}
	rateLimit := defaultWSInboundRate
	if wsInboundRate != 0 {
		rateLimit = wsInboundRate
	}
	if rateLimit < 0 {
		return tools.Fail("ws_inbound_rate must be >= 0 (got %d)", rateLimit), nil
	}
	defaultMaxMsg := defaultChatMaxMsg
	if signaling {
		defaultMaxMsg = defaultSignalMaxMsg // SDP offers with many ICE candidates run 10-40KB.
	}
	maxMsgSize := defaultMaxMsg
	if maxMsgSizeArg != 0 {
		maxMsgSize = maxMsgSizeArg
	}
	if maxMsgSize < 0 || maxMsgSize > maxMaxMsgSize {
		return tools.Fail("max_msg_size must be between 0 and %d (1 MiB; use a WebRTC DataChannel for bulk transfers) (got %d)", maxMaxMsgSize, maxMsgSize), nil
	}

	// Ephemeral rooms default ON for e2ee endpoints: an encrypted call/chat is a
	// throwaway session, so the relay cleans up on room-empty (no client-side delete).
	if cfg.KeepHistory && ephemeralArg {
		return tools.FailCode(tools.CodeBadArg, "keep_history and ephemeral contradict each other: keep_history keeps a room's rows when it empties, ephemeral deletes them"), nil
	}
	ephemeral := ephemeralArg || (e2ee && !cfg.KeepHistory)

	// Room-gated entry defaults ON for e2ee signaling (calls): the password is the E2EE
	// key but does NOT protect peer-to-peer media, so knowing the room name must not be
	// enough to become the second peer. Pure e2ee chat is protected by the key, so the
	// gate is opt-in there. See server/public/ws_gating.go roomJoinGate.
	roomGated := roomGatedArg || (e2ee && signaling)

	// Backstop the mutually-exclusive realtime-model combinations (signaling+tick,
	// matchmaking+room_gated). See validateRealtimeCombos.
	if msg := validateRealtimeCombos(signaling, tickHz, matchmaking, roomGated); msg != "" {
		return tools.Fail("%s", msg), nil
	}

	// Matchmaking: the server pairs peers and hands each a match room to switch
	// into, so it needs room_column (default "room").
	groupSize := defaultGroupSize
	if matchmaking {
		if roomColumn == "" {
			roomColumn = defaultRoomColumn
		}
		if groupSizeArg != 0 {
			groupSize = groupSizeArg
		}
		if groupSize < minGroupSize || groupSize > maxGroupSize {
			return tools.Fail("group_size must be between %d and %d (got %d)", minGroupSize, maxGroupSize, groupSize), nil
		}
	}

	if maxClientsArg < 0 {
		return tools.Fail("max_clients must be >= 0 (got %d)", maxClientsArg), nil
	}
	maxClients := maxClientsArg

	if messageTTLArg < 0 || messageTTLArg > maxMessageTTLSecs {
		return tools.Fail("message_ttl_seconds must be between 0 and %d (1 year) (got %d)", maxMessageTTLSecs, messageTTLArg), nil
	}
	messageTTL := messageTTLArg

	if err := ClaimEndpoint(ctx, endpoints.KindWS, path, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(
		`INSERT INTO of_ws_endpoints (path, write_to_table, room_column, requires_auth, tick_hz, rate_limit, max_msg_size, tenant_column, e2ee, ephemeral, room_gated, matchmaking, group_size, max_clients, message_ttl_seconds, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   write_to_table = excluded.write_to_table,
		   room_column = excluded.room_column,
		   requires_auth = excluded.requires_auth,
		   tick_hz = excluded.tick_hz,
		   rate_limit = excluded.rate_limit,
		   max_msg_size = excluded.max_msg_size,
		   tenant_column = excluded.tenant_column,
		   e2ee = excluded.e2ee,
		   ephemeral = excluded.ephemeral,
		   room_gated = excluded.room_gated,
		   matchmaking = excluded.matchmaking,
		   group_size = excluded.group_size,
		   max_clients = excluded.max_clients,
		   message_ttl_seconds = excluded.message_ttl_seconds,
		   model = excluded.model`,
		path, writeToTable, roomColumn, requiresAuth, tickHz, rateLimit, maxMsgSize, tenantColumn, e2ee, ephemeral, roomGated, matchmaking, groupSize, maxClients, messageTTL, cfg.Model,
	)
	if err != nil {
		return nil, fmt.Errorf("creating websocket endpoint: %w", err)
	}

	// Emit the platform-managed realtime client helper (js/ofnet.js, an ES module
	// imported by specifier "ofnet") so pages call a vetted API instead of re-implementing
	// the handshake. Global scope importmaps it everywhere; platform_managed locks it.
	helperOK := emitRealtimeClientHelper(ctx.DB)

	// When this endpoint enforces e2ee, also emit the platform crypto helper (js/e2ee.js,
	// specifier "e2ee"). The crypto is platform-owned, so the security guarantee does not
	// rest on transcription fidelity: the relay only checks the {iv,ct} envelope is
	// present; this helper makes the ciphertext well-formed.
	e2eeHelperOK := false
	if e2ee {
		e2eeHelperOK = emitE2EEClientHelper(ctx.DB)
	}

	// The CONNECTION example always imports connect() from 'ofnet': on a requires_auth
	// endpoint a raw browser socket cannot authenticate (no Authorization header on WS
	// upgrades; connect() carries the token as ?token=), so leading with raw WebSocket()
	// leaves 401-dead clients. The raw form is a footnote, only on public endpoints.
	usage := buildWSCreateUsage(wsUsageSpec{
		Path:         path,
		RequiresAuth: requiresAuth,
		HelperOK:     helperOK,
		TenantColumn: tenantColumn,
		RoomColumn:   roomColumn,
		Matchmaking:  matchmaking,
		GroupSize:    groupSize,
		MaxClients:   maxClients,
		Signaling:    signaling,
		E2EE:         e2ee,
		RoomGated:    roomGated,
		Ephemeral:    ephemeral,
		WriteToTable: writeToTable,
		MessageTTL:   messageTTL,
		TickHz:       tickHz,
		RateLimit:    rateLimit,
		MaxMsgSize:   maxMsgSize,

		Authoritative: authoritative,
	})

	result := map[string]any{
		"endpoint_path": path,
		"route":         fmt.Sprintf("GET /api/%s/ws", path),
		"requires_auth": requiresAuth,
		"tick_hz":       tickHz,
		"rate_limit":    rateLimit,
		"max_msg_size":  maxMsgSize,
		"usage":         usage,
	}
	if cfg.Model != "" {
		result["model"] = cfg.Model
	}
	if e2ee {
		result["e2ee"] = true
	}
	if ephemeral {
		result["ephemeral"] = true
	}
	if roomGated {
		result["room_gated"] = true
	}
	if matchmaking {
		result["matchmaking"] = true
		result["group_size"] = groupSize
	}
	if maxClients > 0 {
		result["max_clients"] = maxClients
	}
	if messageTTL > 0 {
		result["message_ttl_seconds"] = messageTTL
	}
	if helperOK {
		result["client_helper"] = "js/ofnet.js - platform-managed ES module; import { connect } from 'ofnet'"
	}
	if e2eeHelperOK {
		result["crypto_helper"] = "js/e2ee.js - platform-managed ES module; import { session } from 'e2ee'"
	}
	if mode != "" {
		result["mode"] = mode
	}
	if writeToTable != "" {
		result["write_to_table"] = writeToTable
	}
	if roomColumn != "" {
		result["room_column"] = roomColumn
	}
	if tenantColumn != "" {
		result["tenant_column"] = tenantColumn
	}
	// Self-verify at create time: dial the relay we just mounted and assert it emits the
	// welcome frame, handing the agent live status in the same call (no separate
	// test_websocket round). Best-effort: an un-resolvable host leaves `live` unset
	// rather than claiming the relay broke.
	if tested, live, detail, welcome := wsCreateSmoke(ctx, path, requiresAuth, tenantColumn); tested {
		result["live"] = live
		if live {
			result["welcome"] = welcome
			result["live_note"] = "Dialed the relay and received a welcome frame - it is live and speaking the relay contract. No need to probe it again."
		} else {
			result["live_error"] = detail
			if requiresAuth {
				// The probe dialed WITH a minted token (the ?token= path connect() sends), so a
				// failure here is real, not a "401 because auth-gated" artifact. Name the fix.
				result["live_note"] = "The auth-gated relay did NOT pass a live handshake even with a valid token on ?token= (the browser-equivalent path the 'ofnet' connect() helper sends automatically for a logged-in user). This is a real defect, not the auth gate working - check the relay/reducer, not the auth requirement. Reproduce with manage_testing action=test_websocket endpoint_path=<path> auth_token=<token> (mint the token with manage_testing action=test_auth_flow)."
			} else {
				result["live_note"] = "The relay was created but did NOT pass a live handshake. This is reported once here; re-creating it will not change the result - check the cause (or block) rather than re-probing."
			}
		}
	}
	return tools.Ok(result), nil
}

// wsCreateSmoke dials the freshly mounted relay as an honest first peer and asserts a
// welcome frame arrives, mirroring probe.RunWebSocketSmoke. Returns tested=false
// (not a failure) when no self-test domain is configured; live=false with a detail when
// the relay upgrades but never speaks the contract.
//
// For a requires_auth endpoint it mints a short-lived JWT and dials it on ?token= (the
// path connect() sends for a logged-in user); without this the handshake 401s and reads
// as "relay broken" when the auth gate is simply working. A tenant-scoped endpoint needs
// a token the probe can't synthesize, so it skips the live claim (tested=false).
func wsCreateSmoke(ctx *tools.ToolContext, path string, requiresAuth bool, tenantColumn string) (tested, live bool, detail string, welcome map[string]any) {
	if ctx == nil || ctx.GlobalDB == nil {
		return false, false, "", nil
	}
	port := ctx.PublicPortOrDefault()
	var domain sql.NullString
	_ = ctx.GlobalDB.QueryRow("SELECT domain FROM projects WHERE id = ?", ctx.ProjectID).Scan(&domain) // best-effort; an unreadable domain just skips the probe
	if !domain.Valid || domain.String == "" {
		return false, false, "", nil
	}

	authQ := ""
	if requiresAuth {
		// A tenant-scoped token can't be synthesized here; don't claim broken.
		if tenantColumn != "" || ctx.JWTManager == nil {
			return false, false, "", nil
		}
		// The WS handler validates signature + project match only (not that the user row
		// exists), so a synthetic probe identity authenticates even before users are seeded.
		tok, err := ctx.JWTManager.Generate(security.TokenRequest{ProjectID: ctx.ProjectID, UserID: -1, Username: "of_probe", Role: "user"})
		if err != nil {
			return false, false, "", nil
		}
		authQ = "&token=" + tok
	}

	route := "/api/" + path + "/ws"
	url := fmt.Sprintf("ws://127.0.0.1:%d%s?room=of-probe-%s%s", port, route, path, authQ)

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	conn, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{Host: domain.String})
	if err != nil {
		return true, false, fmt.Sprintf("handshake failed: %v", err), nil
	}
	defer conn.Close(websocket.StatusNormalClosure, "probe done")

	readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRead()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return true, false, "upgraded but emitted no welcome frame within 5s - the relay is not speaking the join/welcome contract", nil
	}
	var frame map[string]any
	if json.Unmarshal(data, &frame) != nil {
		return true, false, "first frame was not valid JSON", nil
	}
	if t, _ := frame["_type"].(string); t != "welcome" {
		return true, false, fmt.Sprintf("first frame was %q, expected a welcome frame", t), nil
	}
	return true, true, "", frame
}

// epUpdateWebSocket patches an endpoint from the per-model tools' shared config. Each field is
// patched only when set (non-empty string / non-zero int / true bool).
func epUpdateWebSocket(ctx *tools.ToolContext, cfg wsEndpointConfig) (*tools.Result, error) {
	writeToTable := cfg.WriteToTable
	roomColumn := cfg.RoomColumn
	tenantColumn := cfg.TenantColumn
	mode := cfg.Mode
	tickHz := cfg.TickHz
	wsInboundRate := cfg.WSInboundRate
	maxMsgSize := cfg.MaxMsgSize
	e2ee := cfg.E2EE
	ephemeral := cfg.Ephemeral
	roomGated := cfg.RoomGated
	matchmaking := cfg.Matchmaking
	groupSize := cfg.GroupSize
	maxClients := cfg.MaxClients
	messageTTL := cfg.MessageTTL

	path := tools.NormalizeEndpointPath(cfg.Path)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	row, err := wsendpoint.Load(ctx.DB, path)
	if err == sql.ErrNoRows {
		return tools.Fail("websocket endpoint %q not found", path), nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading websocket endpoint: %w", err)
	}
	if res := checkModelOwnership(row, cfg.Model); res != nil {
		return res, nil
	}
	// Same rule create enforces, on the state the update would leave: a tenant
	// column needs an authenticated identity to filter on.
	effAuth := row.RequiresAuth
	if cfg.RequiresAuth != nil {
		effAuth = *cfg.RequiresAuth
	}
	effTenant := row.TenantColumn
	if tenantColumn != "" {
		effTenant = tenantColumn
	}
	if effTenant != "" && !effAuth {
		return tools.FailCode(tools.CodeBadArg, "tenant_column requires requires_auth=true (otherwise tenant scoping has no trusted identity to filter on)"), nil
	}

	// mode (test carriers only; the model tools never pass it on update) forces the
	// signaling derivation: tick_hz=0 and the larger SDP frame cap.
	if mode != "" && mode != "chat" && mode != "signaling" {
		return tools.Fail("mode must be 'chat' or 'signaling' (got %q)", mode), nil
	}
	signaling := mode == "signaling"
	// e2ee on a call endpoint implies the invite gate, whether this call switches the
	// endpoint to signaling or it already is one.
	isCall := signaling || row.EffectiveModel() == wsendpoint.ModelSignaling

	// Backstop the mutually-exclusive combos, same rules as create. The effective
	// room_gated mirrors the derivation below (explicit, or implied by e2ee on a call).
	if msg := validateRealtimeCombos(signaling, tickHz, matchmaking, roomGated || (e2ee && isCall)); msg != "" {
		return tools.Fail("%s", msg), nil
	}
	// The stored-row half of the same combos, in both directions: room_gated fixes the
	// room at connect, which disables the in-band switch matched peers rely on.
	if roomGated && row.Matchmaking {
		return tools.Fail("room_gated is incompatible with matchmaking (matched peers switch rooms in-band, which room_gated disables); this endpoint is a matchmaking lobby"), nil
	}
	// An authoritative room's actions are read by the server reducer, so they cannot be
	// end-to-end encrypted (and _action frames are applied before the e2ee check).
	if e2ee && row.EffectiveModel() == wsendpoint.ModelAuthoritative {
		return tools.Fail("e2ee is incompatible with an authoritative endpoint: the server reducer must read every action, so actions cannot be end-to-end encrypted"), nil
	}

	// Matchmaking needs a room_column for matched peers to switch into the assigned
	// room. Default it only when the endpoint has none: an update that merely tunes
	// group_size must not reset a custom room_column back to "room".
	if matchmaking && roomColumn == "" && row.RoomColumn == "" {
		roomColumn = defaultRoomColumn
	}
	if writeToTable != "" || roomColumn != "" {
		effTable, effRoom := row.WriteToTable, row.RoomColumn
		if writeToTable != "" {
			effTable = writeToTable
		}
		if roomColumn != "" {
			effRoom = roomColumn
		}
		if res := validateWSPersistTarget(ctx, effTable, effRoom); res != nil {
			return res, nil
		}
	}

	setClauses := []string{}
	var vals []any

	if writeToTable != "" {
		setClauses = append(setClauses, "write_to_table = ?")
		vals = append(vals, writeToTable)
	}
	if roomColumn != "" {
		setClauses = append(setClauses, "room_column = ?")
		vals = append(vals, roomColumn)
	}
	if tenantColumn != "" {
		if !store.ValidColumnName.MatchString(tenantColumn) {
			return tools.Fail("invalid tenant_column name: %s", tenantColumn), nil
		}
		setClauses = append(setClauses, "tenant_column = ?")
		vals = append(vals, tenantColumn)
	}
	// requires_auth is tri-state: patch only when the caller passed it, so
	// `requires_auth: false` opens an over-gated endpoint back up (a plain-bool merge
	// could never express false).
	if cfg.RequiresAuth != nil {
		setClauses = append(setClauses, "requires_auth = ?")
		vals = append(vals, *cfg.RequiresAuth)
	}
	// tick_hz: switching to signaling with no explicit tick forces 0 (the
	// signaling+tick conflict is rejected up front by validateRealtimeCombos).
	if signaling {
		setClauses = append(setClauses, "tick_hz = ?")
		vals = append(vals, 0)
	} else if tickHz != 0 {
		if tickHz < 0 || tickHz > maxTickHz {
			return tools.Fail("tick_hz must be between 0 and %d (got %d)", maxTickHz, tickHz), nil
		}
		setClauses = append(setClauses, "tick_hz = ?")
		vals = append(vals, tickHz)
	}
	if wsInboundRate != 0 {
		if wsInboundRate < 0 {
			return tools.Fail("ws_inbound_rate must be >= 0 (got %d)", wsInboundRate), nil
		}
		setClauses = append(setClauses, "rate_limit = ?")
		vals = append(vals, wsInboundRate)
	}
	// max_msg_size: signaling bumps the default frame cap to 64KB when the caller
	// didn't set one explicitly.
	effMaxMsg := maxMsgSize
	if signaling && effMaxMsg == 0 {
		effMaxMsg = defaultSignalMaxMsg
	}
	if effMaxMsg != 0 {
		if effMaxMsg < 0 || effMaxMsg > maxMaxMsgSize {
			return tools.Fail("max_msg_size must be between 0 and %d (1 MiB; use a WebRTC DataChannel for bulk transfers) (got %d)", maxMaxMsgSize, effMaxMsg), nil
		}
		setClauses = append(setClauses, "max_msg_size = ?")
		vals = append(vals, effMaxMsg)
	}
	enableE2EE := false
	if e2ee {
		setClauses = append(setClauses, "e2ee = ?")
		vals = append(vals, e2ee)
		enableE2EE = e2ee
	}
	// Ephemeral can be turned on explicitly or implied by enabling e2ee, mirroring
	// the create-time default. Like e2ee, this tool only flips it on (never off).
	switch {
	case cfg.KeepHistory && ephemeral:
		return tools.FailCode(tools.CodeBadArg, "keep_history and ephemeral contradict each other: keep_history keeps a room's rows when it empties, ephemeral deletes them"), nil
	case cfg.KeepHistory:
		setClauses = append(setClauses, "ephemeral = ?")
		vals = append(vals, false)
	case ephemeral || e2ee:
		setClauses = append(setClauses, "ephemeral = ?")
		vals = append(vals, true)
	}
	// Room-gated entry: explicit, or implied by switching this endpoint to an
	// e2ee signaling (call) endpoint. Only flips on, never off.
	if roomGated || (e2ee && isCall) {
		setClauses = append(setClauses, "room_gated = ?")
		vals = append(vals, true)
	}
	// Matchmaking: only flips on. group_size patched when set. Refused on an authoritative
	// endpoint: the room actor disables the in-band room switch matched peers need, so the
	// two cannot share an endpoint (symmetric with the set_reducer guard).
	if matchmaking {
		if row.EffectiveModel() == wsendpoint.ModelAuthoritative {
			return tools.Fail("matchmaking is incompatible with an authoritative reducer (matched peers switch rooms in-band, which the room actor disables). Keep matchmaking and the authoritative game on separate endpoints."), nil
		}
		// Symmetric with the within-call room_gated check: an endpoint already stored
		// room_gated can't have matchmaking turned on later (room_gated fixes the room at
		// connect, disabling the in-band switch matched peers need).
		if row.RoomGated {
			return tools.Fail("matchmaking is incompatible with room_gated (matched peers switch rooms in-band, which room_gated disables); this endpoint is already room_gated"), nil
		}
		setClauses = append(setClauses, "matchmaking = ?")
		vals = append(vals, true)
	}
	if groupSize != 0 {
		if groupSize < minGroupSize || groupSize > maxGroupSize {
			return tools.Fail("group_size must be between %d and %d (got %d)", minGroupSize, maxGroupSize, groupSize), nil
		}
		setClauses = append(setClauses, "group_size = ?")
		vals = append(vals, groupSize)
	}
	if maxClients != nil {
		if *maxClients < 0 {
			return tools.Fail("max_clients must be >= 0 (got %d)", *maxClients), nil
		}
		setClauses = append(setClauses, "max_clients = ?")
		vals = append(vals, *maxClients)
	}
	if messageTTL != 0 {
		if messageTTL < 0 || messageTTL > maxMessageTTLSecs {
			return tools.Fail("message_ttl_seconds must be between 0 and %d (got %d)", maxMessageTTLSecs, messageTTL), nil
		}
		setClauses = append(setClauses, "message_ttl_seconds = ?")
		vals = append(vals, messageTTL)
	}

	if len(setClauses) == 0 {
		return tools.Fail("no fields to update"), nil
	}

	vals = append(vals, path)
	query := fmt.Sprintf("UPDATE of_ws_endpoints SET %s WHERE path = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("updating websocket endpoint: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.Fail("websocket endpoint %q not found", path), nil
	}
	data := map[string]any{"endpoint_path": path, "updated": true}
	if enableE2EE {
		// Enabling e2ee on an existing endpoint emits the platform crypto helper,
		// same as create. Best-effort; the relay enforcement is already live.
		emitE2EEClientHelper(ctx.DB)
		data["next_step"] = "The platform has emitted the crypto helper js/e2ee.js (an ES module - import { session } from 'e2ee') - use its API, do not author it. Follow the e2ee guide's crypto contract."
	}
	return tools.Ok(data), nil
}

// epGetWebSocket returns one endpoint's full config plus its reducer SOURCE - the
// readable counterpart to set_reducer (list only reports has_reducer), so a model
// fixing a reducer bug edits the offending line instead of blind-rewriting.
func epGetWebSocket(ctx *tools.ToolContext, rawPath string) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(rawPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	row, err := wsendpoint.Load(ctx.DB, path)
	if err == sql.ErrNoRows {
		return tools.Fail("websocket endpoint %q not found", path), nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting websocket endpoint: %w", err)
	}
	data := map[string]any{
		"endpoint_path": path, "id": row.ID, "write_to_table": row.WriteToTable, "room_column": row.RoomColumn,
		"tenant_column": row.TenantColumn, "requires_auth": row.RequiresAuth, "tick_hz": row.TickHz,
		"rate_limit": row.RateLimit, "max_msg_size": row.MaxMsgSize, "e2ee": row.E2EE, "ephemeral": row.Ephemeral,
		"room_gated": row.RoomGated, "matchmaking": row.Matchmaking, "group_size": row.GroupSize,
		"max_clients": row.MaxClients, "authoritative": row.Authoritative, "message_ttl_seconds": row.MessageTTLSeconds,
		"created_at": row.CreatedAt, "model": row.EffectiveModel(),
	}
	if row.Reducer != "" {
		data["reducer"] = row.Reducer // full source, so a fix edits the exact line rather than rewriting blind
	}
	if row.SeedTable != "" {
		data["seed_table"] = row.SeedTable
		data["seed_key_column"] = row.SeedKeyColumn
		data["seed_columns"] = row.SeedColumnList()
	}
	if row.SharedTable != "" {
		data["shared_table"] = row.SharedTable
		data["shared_columns"] = row.SharedColumnList()
	}
	return tools.Ok(data), nil
}

func epListWebSocket(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := wsendpoint.LoadAll(ctx.DB)
	if err != nil {
		return nil, fmt.Errorf("listing websocket endpoints: %w", err)
	}
	var endpoints []map[string]any
	for _, row := range rows {
		endpoints = append(endpoints, map[string]any{
			"id": row.ID, "endpoint_path": row.Path, "write_to_table": row.WriteToTable,
			"room_column": row.RoomColumn, "tenant_column": row.TenantColumn, "requires_auth": row.RequiresAuth,
			"tick_hz": row.TickHz, "rate_limit": row.RateLimit, "max_msg_size": row.MaxMsgSize, "e2ee": row.E2EE,
			"ephemeral": row.Ephemeral, "room_gated": row.RoomGated, "matchmaking": row.Matchmaking,
			"group_size": row.GroupSize, "max_clients": row.MaxClients, "authoritative": row.Authoritative,
			"has_reducer": row.HasReducer(), "message_ttl_seconds": row.MessageTTLSeconds, "created_at": row.CreatedAt,
			"model": row.EffectiveModel(),
		})
	}
	return tools.Ok(endpoints), nil
}

func epDeleteWebSocket(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_ws_endpoints", "websocket endpoint", "manage_realtime", "delete", endpointPath)
}
