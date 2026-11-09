package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/internal/projectwrite"
	"openfabrica/internal/roomactor"
	"openfabrica/internal/safego"
	"openfabrica/internal/wsendpoint"
	"openfabrica/store"
)

// roomActor is the authoritative event loop for one room on an authoritative endpoint.
// Exactly one goroutine drains its mailbox, so the reducer never runs concurrently on its
// own state: no locks in user code, deterministic ordering. It owns the state, persists it
// (of_ws_room_state), and pushes each player their view() through the hub.
type roomActor struct {
	key, room, endpoint string
	tenantID            int
	ephemeral           bool
	reducer             *roomactor.Reducer
	reducerSrc          string // guarded by hub.actorsMu, like refs; compared on ensureActor
	hub                 *wsHub
	pdb                 *db.ProjectDB
	mailbox             chan actorMsg
	stopCh              chan struct{} // closed to stop the actor (releaseActor / DrainActors)
	done                chan struct{} // closed when run() returns; DrainActors waits on it
	refs                int           // guarded by hub.actorsMu
	seedBase            uint64
	tickHz              int // >0: post @tick{dt} at this rate for authoritative sims

	// Join-seed config (from the endpoint): on @join the actor reads these columns from
	// seedTable WHERE seedKeyCol = <authenticated uid> and injects them as event.player.seed,
	// so a reducer seeds authoritative state from the DB, not client meta. Empty disables it.
	seedTable   string
	seedKeyCol  string
	seedColumns []string

	// Shared/global read config (from the endpoint): when sharedTable is set the actor reads
	// its rows and injects them as event.shared on every non-@tick event, so a reducer can
	// read cross-room/global state mid-reduce while authority stays per-room. Empty disables it.
	sharedTable   string
	sharedColumns []string

	// run-goroutine-owned (no locking):
	state     json.RawMessage
	rev       int
	evCount   uint64
	players   map[string]json.RawMessage // participants -> presence meta (-> ctx.players)
	uids      map[string]int             // participant -> authenticated user id (0 = anon), server-stamped
	order     []string                   // participant join order (stable ctx.players)
	observers map[string]bool            // spectators: get views, NOT in ctx.players
	timers    map[string]*time.Timer
	lastView  map[string]string // last view JSON sent per client (skip identical)
	// lastPersist is when state was last written to of_ws_room_state; @tick persists
	// at most every tickPersistEvery so a tick-driven phase change survives the room
	// emptying without a write per tick.
	lastPersist time.Time
}

// tickPersistEvery bounds how often a @tick that declares no writes persists state.
const tickPersistEvery = 5 * time.Second

type actorMsgKind int

const (
	amJoin actorMsgKind = iota
	amLeave
	amAction
	amTimer
	amTick
	amReload
)

type actorMsg struct {
	kind      actorMsgKind
	client    string
	userID    int // authenticated user id for amJoin (0 = anonymous); server-stamped
	meta      json.RawMessage
	frame     map[string]any
	timer     string
	dt        float64 // seconds since the previous @tick
	spectator bool
	reducer   *roomactor.Reducer // amReload: compiled off the actor goroutine
	// reloadCfg (amReload, optional) carries the endpoint's refreshed seed/shared config,
	// applied on the actor goroutine that reads it. nil keeps the current config.
	reloadCfg *actorReloadCfg
}

// actorReloadCfg is the per-endpoint reducer context set_reducer can change alongside the
// code. tick_hz and ephemeral stay fixed for an actor's life (they shape its run loop).
type actorReloadCfg struct {
	seedTable     string
	seedKeyCol    string
	seedColumns   []string
	sharedTable   string
	sharedColumns []string
}

func (a *roomActor) join(client string, userID int, meta json.RawMessage, spectator bool) {
	a.post(actorMsg{kind: amJoin, client: client, userID: userID, meta: meta, spectator: spectator})
}
func (a *roomActor) leave(client string) { a.post(actorMsg{kind: amLeave, client: client}) }
func (a *roomActor) action(client string, frame map[string]any) {
	a.post(actorMsg{kind: amAction, client: client, frame: frame})
}

// post enqueues an event for the actor's single goroutine; the kind decides what a full
// mailbox does.
//
// Join, leave, and timer events are NOT droppable: a lost join strands a player, a lost
// leave leaves a ghost, a lost timer stalls a countdown, and none self-heal. So they block
// until the mailbox drains or the actor stops. Each is posted from a goroutine other than
// the actor's own, so the blocking send can't deadlock the drainer against itself, and none
// is posted while holding hub.actorsMu.
//
// Actions and ticks (and a best-effort reducer reload) stay non-blocking: the read pump must
// never block on a busy actor, and a dropped action/tick/reload is cheaper to lose (the
// client resends, the next tick supersedes, the next set_reducer reissues) than to stall on.
func (a *roomActor) post(m actorMsg) {
	switch m.kind {
	case amJoin, amLeave, amTimer:
		select {
		case a.mailbox <- m:
		case <-a.stopCh:
		}
	default:
		select {
		case a.mailbox <- m:
		default:
			slog.Warn("ws room actor mailbox full, event dropped", "room", a.key, "kind", m.kind)
		}
	}
}

func (a *roomActor) run() {
	defer safego.Recover("ws-room-actor")
	defer close(a.done)
	a.loadOrInit()

	// Authoritative tick simulation: a separate goroutine posts @tick{dt} at the endpoint's
	// tick rate so the reducer can advance a sim. Stopped on shutdown.
	//
	// dt is SECONDS, the unit every integrator is written in (x += v * dt); a millisecond dt
	// reads identically at the call site and is wrong by 1000x.
	var tickStop chan struct{}
	if a.tickHz > 0 {
		tickStop = make(chan struct{})
		interval := time.Second / time.Duration(a.tickHz)
		dt := interval.Seconds()
		safego.Go("ws-room-tick", func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-tickStop:
					return
				case <-t.C:
					a.post(actorMsg{kind: amTick, dt: dt})
				}
			}
		})
	}

	// stopCh is closed by releaseActor (last reference) or DrainActors (shutdown). Per-event
	// persistence keeps on-disk state current, so a prompt stop loses nothing durable.
	for {
		select {
		case <-a.stopCh:
			if tickStop != nil {
				close(tickStop)
			}
			a.cancelTimers()
			if a.ephemeral {
				a.deleteState()
			}
			return
		case msg := <-a.mailbox:
			a.safeHandle(msg)
		}
	}
}

// safeHandle wraps handle with a per-event panic recover so a single bad event (a goja host
// panic in reduce/view, a native error) drops just that event instead of unwinding to run()'s
// defer and killing the actor goroutine, which would freeze the room for every client.
func (a *roomActor) safeHandle(msg actorMsg) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("ws room actor event panicked; event dropped, room continues",
				"room", a.key, "kind", msg.kind, "panic", rec)
			if msg.client != "" {
				a.sendErr(msg.client, "internal error")
			}
		}
	}()
	a.handle(msg)
}

// handle applies one mailbox event. Only ever called from the run goroutine, so
// reducer state needs no locking.
func (a *roomActor) handle(msg actorMsg) {
	switch msg.kind {
	case amReload:
		// The endpoint's reducer source changed while this room was live. Restart on the new
		// code: the inherited state was shaped by the old reducer, so replaying the roster from
		// @init is the only transition that leaves it self-consistent.
		a.reducer = msg.reducer
		if c := msg.reloadCfg; c != nil {
			a.seedTable, a.seedKeyCol, a.seedColumns = c.seedTable, c.seedKeyCol, c.seedColumns
			a.sharedTable, a.sharedColumns = c.sharedTable, c.sharedColumns
		}
		a.cancelTimers()
		a.state = nil
		a.rev = 0
		a.lastView = map[string]string{}
		a.apply(map[string]any{"type": "@init"}, "")
		for _, id := range a.order {
			a.apply(map[string]any{"type": "@join", "player": a.playerObjSeeded(id)}, "")
		}
		a.broadcastViews()
	case amJoin:
		if msg.spectator {
			a.observers[msg.client] = true
			a.sendViewTo(msg.client) // observers just watch; send current state
			return
		}
		// A reconnect (same stable client id) re-posts amJoin while the old slot is still
		// held - the superseded connection's leave is suppressed. Refresh the profile but
		// keep ONE roster entry, or ctx.players lists the player twice and the stale copy
		// outlives the eventual leave (removeOrder drops only the first match). @join is
		// still delivered: the reducer contract makes it idempotent for exactly this case.
		_, rejoin := a.players[msg.client]
		a.players[msg.client] = msg.meta
		if msg.userID > 0 {
			a.uids[msg.client] = msg.userID
		}
		if !rejoin {
			a.order = append(a.order, msg.client)
		}
		a.apply(map[string]any{"type": "@join", "player": a.playerObjSeeded(msg.client)}, "")
		a.sendViewTo(msg.client) // guarantee the joiner has state even if @join was a no-op
	case amLeave:
		if a.observers[msg.client] {
			delete(a.observers, msg.client)
			delete(a.lastView, msg.client)
			return
		}
		if _, ok := a.players[msg.client]; !ok {
			return
		}
		a.apply(map[string]any{"type": "@leave", "player": a.playerObj(msg.client)}, "")
		delete(a.players, msg.client)
		delete(a.uids, msg.client)
		delete(a.lastView, msg.client)
		a.removeOrder(msg.client)
	case amAction:
		ev := msg.frame
		// The '@' namespace (@init/@join/@leave/@tick/@timer) is the actor's own lifecycle.
		// Accepting it from the wire would let any client fire a reducer's @timer settle
		// branch, advance the sim with a forged @tick dt, or reset the room with @init -
		// the exact authority this model exists to keep off the client.
		name, _ := ev["_action"].(string)
		if name == "" || strings.HasPrefix(name, "@") {
			a.sendErr(msg.client, "net.action(type): type must be a non-empty app action name; '@'-prefixed events are server-only")
			return
		}
		ev["type"] = name
		delete(ev, "_action")
		delete(ev, "_to")
		// event.shared is server-injected context (shared_table). apply() only overwrites it
		// when a shared read returns rows, so without this a client could forge it.
		delete(ev, "shared")
		ev["_by"] = msg.client
		// An app action reduces as {type, ...payload, _by, player}: net.action spreads the
		// payload at the top level, no `data` wrapper. player is stamped from the connection
		// after the frame is copied in, so it can't be forged, and is the same object
		// @join/@leave carry (a reducer reads identity ONE way).
		ev["player"] = a.playerObj(msg.client)
		a.apply(ev, msg.client)
	case amTimer:
		a.apply(map[string]any{"type": "@timer", "name": msg.timer}, "")
	case amTick:
		a.apply(map[string]any{"type": "@tick", "dt": msg.dt}, "")
	}
}

// apply runs one event through the reducer and, on success, persists the new state and
// pushes every client their view. sender (when non-empty) gets the error on rejection/failure.
func (a *roomActor) apply(event map[string]any, sender string) {
	a.evCount++
	// Inject cross-room/global state as event.shared on non-@tick events; a per-tick global
	// read would hammer the DB for nothing. Action/join/leave/init events carry it.
	if t, _ := event["type"].(string); t != "@tick" {
		if shared := a.readShared(); shared != nil {
			event["shared"] = shared
		}
	}
	seed := a.seedBase ^ (a.evCount * 0x9E3779B97F4A7C15)
	res, err := a.reducer.Reduce(a.state, event, a.playerList(), seed, time.Now().UnixMilli())
	if err != nil {
		// A rejection is business logic: the reducer deliberately refused ONE
		// player's action, so only that player hears about it.
		var rej *roomactor.Rejection
		if errors.As(err, &rej) {
			if sender != "" {
				a.sendErr(sender, rej.Msg)
			}
			return
		}
		// Anything else is a fault. The state does not commit, so an event that faults every
		// time (a bad branch on every tick) freezes the room permanently. Report it to
		// everyone, name the failing event, and keep the log line server-side.
		evType, _ := event["type"].(string)
		a.broadcastErr("reducer failed on " + evType + ": " + err.Error())
		slog.Warn("ws room reducer error", "room", a.key, "event", evType, "error", err)
		return
	}
	a.state = res.State
	a.rev++
	a.applyTimers(res.Timers)
	// A sim ticks tens of times a second, so a plain @tick persists state at most every
	// tickPersistEvery. A @tick that declares writes is a settlement (a countdown hit
	// zero, a round ended): those writes land, with the state that produced them.
	if t, _ := event["type"].(string); t != "@tick" || len(res.Writes) > 0 || time.Since(a.lastPersist) >= tickPersistEvery {
		a.persist()
		a.applyWrites(res.Writes)
	}
	a.broadcastViews()
}

func (a *roomActor) broadcastViews() {
	for _, id := range a.order {
		a.sendViewTo(id)
	}
	for id := range a.observers {
		a.sendViewTo(id)
	}
}

func (a *roomActor) sendViewTo(id string) {
	v, err := a.reducer.View(a.state, id)
	if err != nil {
		// view() is the ONLY thing between a player and everyone else's hidden state, so a
		// projection that throws or times out must send NOTHING rather than the state it was
		// there to withhold: falling back to the full state silently leaks every hand and
		// secret role. Report the fault to that player and hold the frame.
		if a.reducer.HasView() {
			a.sendFaultTo(id, "view() failed for this player, so no state was sent: "+err.Error())
			return
		}
		v = a.state // no view() defined: the whole state IS this player's view
	}
	if a.lastView[id] == string(v) {
		return // unchanged for this player; save the frame
	}
	a.lastView[id] = string(v)
	frame, _ := json.Marshal(map[string]any{
		"_type": "state",
		"_room": a.room,
		"rev":   a.rev,
		"state": v,
	})
	a.hub.sendTo(a.key, id, frame, false)
}

func (a *roomActor) sendErr(id, msg string) {
	frame, _ := json.Marshal(map[string]any{"_type": "error", "_message": msg})
	a.hub.sendTo(a.key, id, frame, false)
}

// sendFaultTo reports a fault to ONE client with the _fault marker the ofnet helper logs, so
// a held state frame surfaces as an error instead of a client that simply stops updating.
func (a *roomActor) sendFaultTo(id, msg string) {
	frame, _ := json.Marshal(map[string]any{"_type": "error", "_message": msg, "_fault": true})
	a.hub.sendTo(a.key, id, frame, false)
}

// broadcastErr reports a reducer FAULT to the whole room. Most faults arrive on @tick,
// @timer, or lifecycle events, which have no sender to reply to, so a sender-only path
// reports them to nobody and the room silently stops advancing.
func (a *roomActor) broadcastErr(msg string) {
	frame, _ := json.Marshal(map[string]any{"_type": "error", "_message": msg, "_fault": true})
	a.hub.broadcastAll(a.key, frame, false)
}

func (a *roomActor) applyTimers(ops []roomactor.TimerOp) {
	for _, op := range ops {
		if t, ok := a.timers[op.Name]; ok {
			t.Stop()
			delete(a.timers, op.Name)
		}
		if op.Set {
			name := op.Name
			a.timers[name] = time.AfterFunc(time.Duration(op.MS)*time.Millisecond, func() {
				a.post(actorMsg{kind: amTimer, timer: name})
			})
		}
	}
}

func (a *roomActor) cancelTimers() {
	for _, t := range a.timers {
		t.Stop()
	}
	a.timers = map[string]*time.Timer{}
}

func (a *roomActor) loadOrInit() {
	var state string
	var rev int
	err := a.pdb.Reader().QueryRow(
		`SELECT state, rev FROM of_ws_room_state WHERE endpoint_path = ? AND tenant_id = ? AND room = ?`,
		a.endpoint, a.tenantID, a.room,
	).Scan(&state, &rev)
	if err == nil && state != "" && state != "null" {
		a.state = json.RawMessage(state)
		a.rev = rev
		return
	}
	a.apply(map[string]any{"type": "@init"}, "")
}

func (a *roomActor) persist() {
	_, err := a.pdb.Writer().Exec(
		`INSERT INTO of_ws_room_state (endpoint_path, tenant_id, room, state, rev, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(endpoint_path, tenant_id, room) DO UPDATE SET
		   state = excluded.state, rev = excluded.rev, updated_at = CURRENT_TIMESTAMP`,
		a.endpoint, a.tenantID, a.room, string(a.state), a.rev,
	)
	if err != nil {
		slog.Warn("ws room state persist failed", "room", a.key, "error", err)
		return
	}
	a.lastPersist = time.Now()
}

func (a *roomActor) deleteState() {
	_, _ = a.pdb.Writer().Exec(
		`DELETE FROM of_ws_room_state WHERE endpoint_path = ? AND tenant_id = ? AND room = ?`,
		a.endpoint, a.tenantID, a.room,
	)
}

func (a *roomActor) playerObj(id string) map[string]any {
	// Parse the stored meta so event.player.meta reaches the reducer as an object, not goja's
	// byte-array wrapping of a raw json.RawMessage (the VM's reflect-based ToValue has no
	// []byte case). Empty meta stays nil (a JS null).
	var meta any
	if raw := a.players[id]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &meta)
	}
	obj := map[string]any{"id": id, "meta": meta}
	if uid := a.uids[id]; uid > 0 {
		obj["uid"] = uid
	}
	return obj
}

// playerObjSeeded is playerObj with the durable DB seed injected (keyed on the authed uid) so
// the reducer seeds authoritative balances from the DB, not client meta. Used on a fresh join
// AND on the @reload roster replay. readSeed returns nil when unconfigured or anonymous.
func (a *roomActor) playerObjSeeded(id string) map[string]any {
	player := a.playerObj(id)
	if seed := a.readSeed(id); seed != nil {
		player["seed"] = seed
	}
	return player
}

func (a *roomActor) playerList() []roomactor.Player {
	out := make([]roomactor.Player, 0, len(a.order))
	for _, id := range a.order {
		out = append(out, roomactor.Player{ID: id, UID: a.uids[id], Meta: a.players[id]})
	}
	return out
}

// readSeed loads the durable seed row for a joining client, keyed on its server-stamped
// authenticated uid (never client meta). Returns nil when seeding is unconfigured, the
// connection is anonymous, or the row is absent (the reducer then applies its own default).
// Identifiers are re-validated defensively.
func (a *roomActor) readSeed(client string) map[string]any {
	if a.seedTable == "" || len(a.seedColumns) == 0 {
		return nil
	}
	uid := a.uids[client]
	if uid <= 0 {
		return nil
	}
	if !store.ValidTableName.MatchString(a.seedTable) || !store.ValidColumnName.MatchString(a.seedKeyCol) {
		return nil
	}
	cols := make([]string, 0, len(a.seedColumns))
	for _, c := range a.seedColumns {
		if store.ValidColumnName.MatchString(c) {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return nil
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ? LIMIT 1",
		strings.Join(cols, ", "), a.seedTable, a.seedKeyCol)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := a.pdb.Reader().QueryRow(query, uid).Scan(ptrs...); err != nil {
		return nil // no such player yet (or a read error) -> reducer uses its default
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		// SQLite hands TEXT back as []byte via interface{} scan; stringify so the
		// value JSON-encodes as a string (not base64) into the event.
		if b, ok := vals[i].([]byte); ok {
			out[c] = string(b)
		} else {
			out[c] = vals[i]
		}
	}
	return out
}

// maxSharedRows caps how many rows readShared injects, so a large shared table
// can't bloat an event or the reduce budget.
const maxSharedRows = 200

// readShared loads the endpoint's shared table (all rows, capped) as column->value maps,
// injected into each non-@tick event as event.shared so a reducer can read cross-room/global
// state mid-reduce. Returns nil when unconfigured or on any read error; identifiers re-validated.
func (a *roomActor) readShared() []map[string]any {
	if a.sharedTable == "" || !store.ValidTableName.MatchString(a.sharedTable) {
		return nil
	}
	colSpec := "*"
	if len(a.sharedColumns) > 0 {
		cols := make([]string, 0, len(a.sharedColumns))
		for _, c := range a.sharedColumns {
			if !store.ValidColumnName.MatchString(c) {
				return nil
			}
			cols = append(cols, c)
		}
		colSpec = strings.Join(cols, ", ")
	} else {
		// "*" would inject PASSWORD/ENCRYPTED values into event.shared, where a reducer could
		// fold them into view() output. Expand to non-secure columns; fail closed if we can't.
		secure, err := store.LoadSecureColumns(a.pdb.Reader(), a.sharedTable)
		if err != nil {
			// An unreadable projection means we can't tell which columns are secret, so "*"
			// is not a safe fallback. (An unregistered table reports an empty projection.)
			return nil
		}
		if len(secure) > 0 {
			lowerSecure := make(map[string]struct{}, len(secure))
			for c := range secure {
				lowerSecure[strings.ToLower(c)] = struct{}{}
			}
			schema, serr := store.LoadTableSchema(a.pdb.Reader(), a.sharedTable)
			if serr != nil || len(schema) == 0 {
				return nil
			}
			cols := make([]string, 0, len(schema))
			for c := range schema {
				if _, isSecure := lowerSecure[strings.ToLower(c)]; isSecure {
					continue
				}
				if !store.ValidColumnName.MatchString(c) {
					return nil
				}
				cols = append(cols, c)
			}
			if len(cols) == 0 {
				return nil
			}
			colSpec = strings.Join(cols, ", ")
		}
	}
	query := fmt.Sprintf("SELECT %s FROM %s LIMIT %d", colSpec, a.sharedTable, maxSharedRows)
	rows, err := a.pdb.Reader().Query(query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	colNames, err := rows.Columns()
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		m := make(map[string]any, len(colNames))
		for i, c := range colNames {
			if b, ok := vals[i].([]byte); ok {
				m[c] = string(b)
			} else {
				m[c] = vals[i]
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return out
}

// applyWrites performs the durable writes a reducer declared via ctx.insert / ctx.update
// after a successful reduce. Hardening lives here: reject of_* system tables and any write
// touching a PASSWORD/ENCRYPTED column (the actor has no Encryptor, so it would store a raw
// secret). All accepted writes run in ONE transaction; a failure is logged and the room keeps running.
func (a *roomActor) applyWrites(writes []roomactor.Write) {
	if len(writes) == 0 {
		return
	}
	checked := make([]roomactor.Write, 0, len(writes))
	for _, w := range writes {
		if _, err := store.SanitizedTableName(w.Table); err != nil {
			slog.Warn("reducer write rejected", "room", a.key, "table", w.Table, "error", err)
			continue
		}
		if secure, err := store.LoadSecureColumns(a.pdb.Reader(), w.Table); err == nil && touchesSecureColumn(w, secure) {
			slog.Warn("reducer write rejected: targets a PASSWORD/ENCRYPTED column", "room", a.key, "table", w.Table)
			continue
		}
		checked = append(checked, w)
	}
	if len(checked) == 0 {
		return
	}
	err := a.pdb.WithTx(func(tx *sql.Tx) error {
		for _, w := range checked {
			var e error
			switch w.Op {
			case "insert":
				_, e = projectwrite.Insert(tx, w.Table, w.Row)
			case "update":
				_, e = projectwrite.Update(tx, w.Table, w.Row, w.Where)
			case "upsert":
				_, e = projectwrite.Upsert(tx, w.Table, w.Row, w.Conflict)
			default:
				e = fmt.Errorf("unknown reducer write op %q", w.Op)
			}
			if e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("reducer settlement write failed", "room", a.key, "error", err)
	}
}

// touchesSecureColumn reports whether a write assigns to a PASSWORD/ENCRYPTED
// column (checked against the write's SET/insert columns, i.e. Row keys).
func touchesSecureColumn(w roomactor.Write, secure map[string]string) bool {
	// SQLite column names are case-insensitive, so fold both sides: a reducer can't dodge the
	// PASSWORD/ENCRYPTED guard (which would store a raw secret) by varying case.
	lowered := make(map[string]struct{}, len(secure))
	for col := range secure {
		lowered[strings.ToLower(col)] = struct{}{}
	}
	for col := range w.Row {
		if _, ok := lowered[strings.ToLower(col)]; ok {
			return true
		}
	}
	return false
}

func (a *roomActor) removeOrder(id string) {
	for i, v := range a.order {
		if v == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			return
		}
	}
}

// ensureActor returns the room actor for an authoritative endpoint, spawning it (compiling
// the reducer once) on first use and bumping its ref count. ok=false if the reducer fails to
// compile (caller falls back to a plain relay). Reference-counted so spawn/teardown are
// race-free under actorsMu.
// actorSpec carries the room + reducer + seed context for ensureActor, replacing a
// 14-parameter positional signature.
type actorSpec struct {
	Key           string
	Room          string
	Endpoint      string
	TenantID      int
	ReducerSrc    string
	Ephemeral     bool
	TickHz        int
	SeedTable     string
	SeedKeyCol    string
	SeedColumns   []string
	SharedTable   string
	SharedColumns []string
	PDB           *db.ProjectDB
}

func (h *wsHub) ensureActor(spec actorSpec) (*roomActor, bool) {
	key := spec.Key
	room := spec.Room
	endpoint := spec.Endpoint
	tenantID := spec.TenantID
	reducerSrc := spec.ReducerSrc
	ephemeral := spec.Ephemeral
	tickHz := spec.TickHz
	seedTable := spec.SeedTable
	seedKeyCol := spec.SeedKeyCol
	seedColumns := spec.SeedColumns
	sharedTable := spec.SharedTable
	sharedColumns := spec.SharedColumns
	pdb := spec.PDB

	red, err := roomactor.Compile(reducerSrc)
	if err != nil {
		slog.Warn("authoritative endpoint reducer failed to compile; falling back to relay", "endpoint", endpoint, "error", err)
		return nil, false
	}

	h.actorsMu.Lock()
	defer h.actorsMu.Unlock()
	if a := h.actors[key]; a != nil {
		a.refs++
		// Hand a live room the new code rather than the reducer it spawned with.
		// The swap runs on the actor goroutine, which owns the reducer field.
		if a.reducerSrc != reducerSrc {
			a.reducerSrc = reducerSrc
			a.post(actorMsg{kind: amReload, reducer: red})
		}
		return a, true
	}
	a := &roomActor{
		key: key, room: room, endpoint: endpoint, tenantID: tenantID, ephemeral: ephemeral,
		reducer: red, reducerSrc: reducerSrc, hub: h, pdb: pdb, mailbox: make(chan actorMsg, 64), refs: 1,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
		tickHz:        tickHz,
		seedTable:     seedTable,
		seedKeyCol:    seedKeyCol,
		seedColumns:   seedColumns,
		sharedTable:   sharedTable,
		sharedColumns: sharedColumns,
		seedBase:      seedFor(key),
		players:       map[string]json.RawMessage{},
		uids:          map[string]int{},
		observers:     map[string]bool{},
		timers:        map[string]*time.Timer{},
		lastView:      map[string]string{},
	}
	h.actors[key] = a
	safego.Go("ws-room-actor", a.run)
	return a, true
}

// ReloadEndpointReducer hands every LIVE room of one project endpoint its current reducer
// (and seed/shared config), re-read from that project's DB. Without it a set_reducer fix
// reached a live room only when the next player joined, so re-testing an open room kept
// showing the old behaviour. Driven by events.EventWSReducerChanged.
func (h *wsHub) ReloadEndpointReducer(projectID int, endpoint string) {
	prefix := fmt.Sprintf("%d:", projectID)
	h.actorsMu.Lock()
	defer h.actorsMu.Unlock()
	var (
		loaded bool
		row    *wsendpoint.Row
		red    *roomactor.Reducer
	)
	for key, a := range h.actors {
		if a.endpoint != endpoint || !strings.HasPrefix(key, prefix) || a.pdb == nil {
			continue
		}
		if !loaded {
			loaded = true
			r, err := wsendpoint.Load(a.pdb.Reader(), endpoint)
			if err != nil || r.Reducer == "" {
				return
			}
			compiled, err := roomactor.Compile(r.Reducer)
			if err != nil {
				slog.Warn("reducer reload: new reducer failed to compile; live rooms keep the old one", "endpoint", endpoint, "error", err)
				return
			}
			row, red = r, compiled
		}
		if a.reducerSrc == row.Reducer {
			continue // same code: a reload would only reset the room (ensureActor's rule too)
		}
		a.reducerSrc = row.Reducer
		a.post(actorMsg{kind: amReload, reducer: red, reloadCfg: &actorReloadCfg{
			seedTable: row.SeedTable, seedKeyCol: row.SeedKeyColumn, seedColumns: row.SeedColumnList(),
			sharedTable: row.SharedTable, sharedColumns: row.SharedColumnList(),
		}})
	}
}

// releaseActor drops one reference; when the last connection leaves, the actor is removed
// from the registry and stopped. Closing stopCh (not a droppable mailbox sentinel) makes the stop reliable.
func (h *wsHub) releaseActor(key string) {
	h.actorsMu.Lock()
	defer h.actorsMu.Unlock()
	a := h.actors[key]
	if a == nil {
		return
	}
	a.refs--
	if a.refs <= 0 {
		delete(h.actors, key)
		close(a.stopCh)
	}
}

// DrainActors stops every live room actor and waits (until ctx is done) for each to finish,
// so their final state is flushed before the project DBs close. Called on shutdown AFTER the
// HTTP listeners stop and BEFORE ProjectDBManager.CloseAll. Each actor is removed under the
// lock before its stopCh is closed, so it can never be closed twice.
func (h *wsHub) DrainActors(ctx context.Context) {
	h.actorsMu.Lock()
	actors := make([]*roomActor, 0, len(h.actors))
	for k, a := range h.actors {
		actors = append(actors, a)
		delete(h.actors, k)
		close(a.stopCh)
	}
	h.actorsMu.Unlock()

	for _, a := range actors {
		select {
		case <-a.done:
		case <-ctx.Done():
			return
		}
	}
}

func seedFor(key string) uint64 {
	hsh := fnv.New64a()
	_, _ = hsh.Write([]byte(key))
	return hsh.Sum64()
}
