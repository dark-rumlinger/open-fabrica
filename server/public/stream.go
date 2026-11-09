package public

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"openfabrica/events"
	"openfabrica/internal/httpx"
	"openfabrica/store"
)

type streamEndpointConfig struct {
	Path         string
	EventTypes   []string
	TableName    string // when set, only push events whose payload "table" matches
	RequiresAuth bool
}

func (h *Handler) loadStreamEndpoint(projectDB *sql.DB, path string) (*streamEndpointConfig, error) {
	var se streamEndpointConfig
	var eventTypesJSON string
	var tableName sql.NullString
	err := projectDB.QueryRow(
		"SELECT path, event_types, table_name, requires_auth FROM of_stream_endpoints WHERE path = ?",
		path,
	).Scan(&se.Path, &eventTypesJSON, &tableName, &se.RequiresAuth)
	if err != nil {
		return nil, err
	}
	se.TableName = tableName.String
	if eventTypesJSON != "" {
		if err := json.Unmarshal([]byte(eventTypesJSON), &se.EventTypes); err != nil {
			slog.Warn("invalid event_types JSON in stream endpoint", "path", path, "error", err)
		}
	}
	// Default to standard data events if none configured.
	if len(se.EventTypes) == 0 {
		se.EventTypes = []string{"data.insert", "data.update", "data.delete"}
	}
	return &se, nil
}

// apiStream handles GET /api/{path}/stream: SSE for real-time data changes.
func (h *Handler) apiStream(w http.ResponseWriter, r *http.Request, projectID int, projectDB *sql.DB) {
	// Extract the endpoint path from the URL (strip trailing "/stream" sub-route).
	fullPath := strings.TrimPrefix(r.URL.Path, "/api/")
	endpointPath := strings.TrimSuffix(fullPath, "/stream")

	se, err := h.loadStreamEndpoint(projectDB, endpointPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "stream endpoint not found")
		return
	}

	// Resolve the caller's identity whether or not the endpoint requires auth: an
	// unauthenticated subscriber must still be scoped OUT of owner- and tenant-owned
	// rows, and a public stream over a public table needs no identity. EventSource
	// cannot set headers, so the token may ride the query.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	valid, claims := h.validateProjectTokenOrJWTWithClaims(projectID, token)
	if se.RequiresAuth && !valid {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	scope := &streamScope{
		h:            h,
		projectDB:    projectDB,
		isProjectKey: valid && claims == nil,
		cache:        map[string]*apiEndpoint{},
	}
	if claims != nil {
		scope.userID, scope.userRole = claims.UserID, claims.Role
		scope.tenantID, scope.tenantRole = claims.TenantID, claims.TenantRole
	}

	sse, ok := httpx.NewSSE(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Build a set of allowed event types for fast lookup.
	allowedTypes := make(map[events.EventType]bool)
	for _, et := range se.EventTypes {
		allowedTypes[events.EventType(et)] = true
	}

	// pass is the ONE filter every event clears before this stream emits it - the
	// same decision for a live event and a backfilled one: this project, an allowed
	// type, the scoped table (if any), and the row-level scope check.
	pass := func(e events.Event) bool {
		if e.ProjectID != projectID || !allowedTypes[e.Type] {
			return false
		}
		// When the endpoint is scoped to one table, only push row events whose
		// payload "table" matches; other tables' events are not this stream's.
		// The "endpoint" key is accepted too: some projects follow an older manage_sse
		// wording where table_name was the create_api endpoint path, so they carry a
		// route here and would otherwise stream nothing at all, silently and forever.
		if se.TableName != "" {
			t, _ := e.Payload["table"].(string)
			ep, _ := e.Payload["endpoint"].(string)
			if t != se.TableName && ep != se.TableName {
				return false
			}
		}
		// Row-level scoping. A row event carries the row's own fields, so a stream
		// over an owner- or tenant-scoped table would hand every subscriber every
		// other user's and tenant's data unless filtered here - the same leak the REST
		// path closes with its SQL predicate and the WS path by folding tenant into
		// the room key.
		return scope.allow(e.Type, e.Payload)
	}

	// Subscribe BEFORE reading the backfill so no event slips through the gap
	// between the two: an event published during setup is delivered live here AND
	// captured by Since below, and the id-based dedup in the loop drops the copy.
	eventCh := make(chan events.Event, 64)
	subID := h.deps.Bus.SubscribeAll(func(e events.Event) {
		if !pass(e) {
			return
		}
		select {
		case eventCh <- e:
		default:
			slog.Warn("SSE event dropped (channel full)", "project_id", projectID, "event", e.Type)
		}
	})
	defer h.deps.Bus.Unsubscribe(subID)

	// Reconnect backfill: EventSource echoes the last id it saw as Last-Event-ID
	// (some clients pass ?lastEventId=). Replay retained events after it, filtered
	// exactly like a live event, so a reconnecting client misses no gap. backfillMax
	// is the replay high-water; a live event at or below it was already covered and
	// is skipped to avoid a duplicate.
	var backfillMax uint64
	if lastSeen := parseLastEventID(r); lastSeen > 0 {
		// The retained window is a bounded ring shared project-wide, so a long
		// disconnect can outlive it. Say so: the replay below is then partial, and
		// the client has no way to tell that from a clean reconnect.
		if oldest := h.deps.Bus.OldestID(); oldest > lastSeen+1 {
			slog.Warn("SSE backfill gap: events aged out of the retained window before the client reconnected",
				"project_id", projectID, "path", endpointPath, "last_seen", lastSeen, "oldest_retained", oldest)
		}
		for _, e := range h.deps.Bus.Since(lastSeen) {
			backfillMax = e.ID // Since returns publish order, so this ends at the max
			if !pass(e) {
				continue
			}
			data, err := json.Marshal(e.Payload)
			if err != nil {
				continue
			}
			if sse.EventID(e.ID, string(e.Type), data) != nil {
				return // client already gone
			}
		}
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-eventCh:
			if e.ID <= backfillMax {
				continue // already delivered (or filtered) during backfill
			}
			data, err := json.Marshal(e.Payload)
			if err != nil {
				slog.Warn("SSE marshal error", "project_id", projectID, "event", e.Type, "error", err)
				continue
			}
			if sse.EventID(e.ID, string(e.Type), data) != nil {
				return
			}
		case <-ticker.C:
			if sse.Comment("keepalive") != nil {
				return
			}
		}
	}
}

// parseLastEventID reads an EventSource's reconnection cursor: the Last-Event-ID
// header, falling back to a ?lastEventId= query param for a client that cannot set
// the header. Returns 0 (no backfill) when absent or unparseable.
func parseLastEventID(r *http.Request) uint64 {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("lastEventId")
	}
	id, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// streamScope applies row-level security to an SSE subscription, reusing the
// decision the plan already made on the emitting endpoint: a row event carries the
// `endpoint` that published it, so the stream scopes exactly as that endpoint's REST
// reads do. An endpoint with no owner/tenant column is unscoped.
//
// The bus fans each event out inline on the publishing goroutine, and publishes
// arrive from many goroutines at once, so one subscription's callback can run
// concurrently with itself. mu guards the endpoint cache against that.
type streamScope struct {
	h            *Handler
	projectDB    *sql.DB
	userID       int
	userRole     string
	tenantID     int
	tenantRole   string
	isProjectKey bool
	mu           sync.Mutex
	cache        map[string]*apiEndpoint
	scopedKnown  bool
	anyScoped    bool
}

// endpointFor resolves and memoises the emitting endpoint's config. A miss is
// cached as nil so a deleted endpoint isn't re-queried per event. Held under mu
// because concurrent publishes call this on the same streamScope.
func (s *streamScope) endpointFor(path string) *apiEndpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ep, seen := s.cache[path]; seen {
		return ep
	}
	ep, err := s.h.loadEndpoint(s.projectDB, path)
	if err != nil {
		ep = nil
	}
	s.cache[path] = ep
	return ep
}

// projectHasScopedEndpoint reports whether ANY API endpoint in this project
// restricts rows by owner or tenant, memoised for the life of the connection. It
// is what decides how an unattributable row event is treated: in a project where
// nothing is scoped there is nothing to leak and the event passes, which keeps
// custom publishes (a schedule raising its own data.insert) working as documented.
func (s *streamScope) projectHasScopedEndpoint() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopedKnown {
		return s.anyScoped
	}
	s.scopedKnown = true
	// A read error answers "scoped": withholding an unattributable event is the
	// safe side of a database we could not consult.
	s.anyScoped = true
	var n int
	if err := s.projectDB.QueryRow(
		`SELECT COUNT(*) FROM of_api_endpoints WHERE COALESCE(owner_column,'') != '' OR COALESCE(tenant_column,'') != ''`,
	).Scan(&n); err == nil {
		s.anyScoped = n > 0
	}
	return s.anyScoped
}

func (s *streamScope) allow(evType events.EventType, payload map[string]any) bool {
	endpointPath, _ := payload["endpoint"].(string)
	if endpointPath == "" {
		// No endpoint means no scoping decision to inherit. For a NON-row event (a
		// job, a webhook, a custom event name) that is genuinely out of scope and
		// passes. For a data.* event it is not: the CRUD path always stamps
		// "endpoint", so an unattributed row event is a custom publish, and letting
		// it through would hand every subscriber rows the owner/tenant filter below
		// exists to withhold. Fail closed and say why - the publisher fixes it by
		// including "endpoint" in the payload.
		if strings.HasPrefix(string(evType), "data.") && s.projectHasScopedEndpoint() {
			slog.Warn("SSE: row event published without an \"endpoint\" key; withheld because this project scopes rows and the event cannot be attributed",
				"event_type", evType)
			return false
		}
		return true
	}
	ep := s.endpointFor(endpointPath)
	if ep == nil {
		return false // the endpoint is gone: we cannot tell who may see this row
	}
	// Mirror the REST read gate before any row-level decision: an endpoint that
	// requires auth (and does not expose public reads) must not stream to a
	// subscriber who could not GET it, and a required_role must hold here as well.
	// A project key keeps its owner-level access, admin satisfies every role gate.
	if ep.RequiresAuth && !ep.PublicRead && !s.isProjectKey {
		if s.userID == 0 {
			return false
		}
		if ep.RequiredRole != "" && s.userRole != ep.RequiredRole && s.userRole != "admin" {
			return false
		}
	}
	if ep.OwnerColumn == "" && ep.TenantColumn == "" {
		return true // unscoped endpoint, unscoped stream
	}
	// The same bypasses the REST path grants: a project API key has owner-level
	// access, and public_read endpoints expose unscoped reads on purpose.
	if s.isProjectKey || ep.PublicRead {
		return true
	}
	// authored owner_scope shares reads, so the live stream mirrors the REST GET:
	// the owner column attributes writes, not read visibility. The tenant filter
	// below still scopes the stream to the caller's tenant.
	authoredShare := ep.OwnerScope == store.OwnerScopeAuthored
	if ep.OwnerColumn != "" && !authoredShare && s.userRole != "admin" {
		v, ok := payload[ep.OwnerColumn]
		// A payload that does not carry the owner column cannot be attributed - a
		// delete event, for one, carries only the row id. Fail closed.
		if !ok || s.userID == 0 || !sameID(v, s.userID) {
			return false
		}
	}
	if ep.TenantColumn != "" && s.tenantRole != "platform_admin" {
		v, ok := payload[ep.TenantColumn]
		if !ok || s.tenantID == 0 || !sameID(v, s.tenantID) {
			return false
		}
	}
	return true
}

// sameID compares a payload value against an identity from the token. Payload
// numbers arrive as float64 (parsed request JSON), int64 (read back from SQLite),
// or a string, so the comparison normalises rather than trusting one shape.
func sameID(v any, want int) bool {
	switch n := v.(type) {
	case float64:
		return int(n) == want
	case float32:
		return int(n) == want
	case int:
		return n == want
	case int32:
		return int(n) == want
	case int64:
		return int(n) == want
	case json.Number:
		i, err := n.Int64()
		return err == nil && int(i) == want
	case string:
		i, err := strconv.Atoi(n)
		return err == nil && i == want
	default:
		return false
	}
}
