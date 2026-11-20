package public

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/internal/funcexec"
	"openfabrica/internal/mqtt"
	"openfabrica/internal/tokenrevoke"
	"openfabrica/security"
	"openfabrica/store"
)

const (
	mqttTouchThrottle   = 30 * time.Second
	mqttPruneThrottle   = 15 * time.Minute
	mqttDefaultFnBudget = 10 * time.Second
)

var mqttIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// HashMQTTToken returns the hex-encoded SHA-256 digest stored in of_mqtt_devices.secret_hash.
func HashMQTTToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func checkMQTTToken(expectedHash, provided string) bool {
	if expectedHash == "" || provided == "" {
		return false
	}
	got := HashMQTTToken(provided)
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(got)) == 1
}

// openExistingProjectDB opens a project DB only if it already exists on disk or
// in cache, avoiding conjuring empty project directories while surviving idle eviction.
func openExistingProjectDB(mgr *db.ProjectDBManager, projectID int) *db.ProjectDB {
	if mgr == nil || !mgr.Exists(projectID) {
		return nil
	}
	pdb, err := mgr.Open(projectID)
	if err != nil {
		return nil
	}
	return pdb
}

// NewMQTTHub creates the shared multi-project MQTT hub wired to project DBs,
// function execution, and the event bus, and registers ctx.mqtt.publish.
func NewMQTTHub(deps *Deps) *mqtt.Hub {
	hub := mqtt.NewHub(mqtt.ProjectHooks{
		Enabled: func(projectID int) bool {
			pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
			if pdb == nil {
				return false
			}
			cfg, err := store.LoadMQTTConfig(pdb.Reader())
			return err == nil && cfg.Enabled
		},
		LoadConfig: func(projectID int) (mqtt.BrokerConfig, error) {
			pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
			if pdb == nil {
				return mqtt.BrokerConfig{}, sql.ErrNoRows
			}
			cfg, err := store.LoadMQTTConfig(pdb.Reader())
			if err != nil {
				return mqtt.BrokerConfig{}, err
			}
			rules, err := store.ListMQTTRules(pdb.Reader())
			if err != nil {
				rules = nil
			}
			bridges := loadProjectBridgeConfigs(pdb.Reader(), deps.Encryptor, projectID)
			return mqtt.BrokerConfig{
				MaxClients:     cfg.MaxClients,
				Retained:       &sqlRetainedStore{mgr: deps.ProjectDBManager, projectID: projectID},
				Sessions:       &sqlSessionStore{mgr: deps.ProjectDBManager, projectID: projectID},
				Auth:           makeMQTTAuth(deps, projectID, cfg),
				OnPublish:      makeMQTTPublishHook(deps, projectID, rules),
				OnConnect:      makeMQTTClientHook(deps, projectID, events.EventMQTTClientConnected),
				OnDisconnect:   makeMQTTClientHook(deps, projectID, events.EventMQTTClientDisconnected),
				OnWatchdogTick: makeMQTTWatchdogTick(deps, projectID),
				HasWatchdogs:   store.HasMQTTWatchdogs(pdb.Reader()),
				Bridges:        bridges,
			}, nil
		},
		ResolveTCPConn: func(username, password string) (int, bool) {
			return resolveTCPProject(deps, username, password)
		},
	})

	funcexec.RegisterMQTTPublish(func(projectID int, topic string, payload []byte, qos byte, retain bool) error {
		b, err := hub.BrokerFor(projectID)
		if err != nil {
			return err
		}
		if b == nil {
			return http.ErrNotSupported
		}
		return b.Publish(topic, payload, qos, retain)
	})
	funcexec.RegisterMQTTStats(func(projectID int) (map[string]any, bool) {
		st, ok := hub.StatsFor(projectID)
		if !ok {
			return nil, false
		}
		return map[string]any{
			"connected_clients": st.ConnectedClients,
			"client_ids":        st.ClientIDs,
			"clients":           st.Clients,
			"subscriptions":     st.Subscriptions,
			"bridges":           st.Bridges,
		}, true
	})
	return hub
}

// StartMQTTBridges starts brokers for any enabled project that has configured
// outbound bridges so LAN/remote bridges reconnect automatically on server boot.
func StartMQTTBridges(deps *Deps, hub *mqtt.Hub) {
	if deps == nil || deps.DB == nil || hub == nil {
		return
	}
	ids, err := store.ListProjectIDs(deps.DB.DB)
	if err != nil || len(ids) == 0 {
		return
	}
	hub.StartBridgedProjects(ids)
}

func loadProjectBridgeConfigs(reader *sql.DB, enc *security.Encryptor, projectID int) []mqtt.BridgeConfig {
	rows, err := store.ListMQTTBridges(reader)
	if err != nil || len(rows) == 0 {
		return nil
	}
	out := make([]mqtt.BridgeConfig, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		var pw string
		if r.PasswordEnc != "" && enc != nil {
			if dec, derr := enc.OpenOptional(r.PasswordEnc); derr == nil {
				pw = dec
			}
		}
		cid := strings.TrimSpace(r.ClientID)
		if cid == "" {
			cid = fmt.Sprintf("of-p%d-%s", projectID, r.Name)
		}
		out = append(out, mqtt.BridgeConfig{
			Name:         r.Name,
			BrokerURL:    r.BrokerURL,
			ClientID:     cid,
			Username:     r.Username,
			Password:     pw,
			RemoteSub:    splitBridgeCSV(r.RemoteSub),
			LocalPrefix:  r.LocalPrefix,
			LocalPub:     splitBridgeCSV(r.LocalPub),
			RemotePrefix: r.RemotePrefix,
			QoS:          r.QoS,
		})
	}
	return out
}

func splitBridgeCSV(s string) []string {
	var out []string
	for _, raw := range strings.Split(s, ",") {
		if f := strings.TrimSpace(raw); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func expandUserMQTTACL(pattern string, claims *security.Claims) string {
	if claims == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(claims.Role)) {
	case "admin", "owner", "platform_admin":
		return "#"
	}
	var out []string
	for _, raw := range strings.Split(pattern, ",") {
		f := strings.TrimSpace(raw)
		if f == "" || strings.EqualFold(f, "none") {
			continue
		}
		if strings.Contains(f, "{user_id}") {
			if claims.UserID <= 0 {
				continue
			}
			f = strings.ReplaceAll(f, "{user_id}", strconv.Itoa(claims.UserID))
		}
		if strings.Contains(f, "{tenant_id}") {
			if claims.TenantID <= 0 {
				continue
			}
			f = strings.ReplaceAll(f, "{tenant_id}", strconv.Itoa(claims.TenantID))
		}
		out = append(out, f)
	}
	return strings.Join(out, ",")
}

func makeMQTTAuth(deps *Deps, projectID int, cfg store.MQTTConfig) mqtt.Authenticator {
	return func(clientID, username, password string) (mqtt.AuthResult, bool) {
		pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
		if pdb == nil {
			return mqtt.AuthResult{}, false
		}
		// Strip optional "<project>:" prefix when routed from raw TCP.
		if _, rest, ok := strings.Cut(username, ":"); ok {
			username = rest
		}
		if username != "" {
			if dev, ok, err := store.LookupMQTTDevice(pdb.Reader(), username); err == nil && ok {
				if checkMQTTToken(dev.SecretHash, password) {
					touchDeviceAndEmitRecovered(deps, projectID, pdb.Writer(), dev.Username)
					return mqtt.AuthResult{
						DeviceName: dev.Name,
						Username:   dev.Username,
						AllowPub:   dev.AllowPub,
						AllowSub:   dev.AllowSub,
					}, true
				}
				return mqtt.AuthResult{}, false
			}
		}
		// Allow signed-in project users connecting over WebSocket with a JWT token.
		if deps.JWTManager != nil && password != "" {
			if claims, err := deps.JWTManager.Validate(password); err == nil && claims != nil && claims.ProjectID == projectID {
				var issuedAt *time.Time
				if claims.IssuedAt != nil {
					t := claims.IssuedAt.Time
					issuedAt = &t
				}
				if claims.UserID > 0 && tokenrevoke.Revoked(pdb.Reader(), projectID, claims.UserID, issuedAt) {
					return mqtt.AuthResult{}, false
				}
				ident := strconv.Itoa(claims.UserID)
				return mqtt.AuthResult{
					DeviceName: ident,
					Username:   ident,
					AllowPub:   expandUserMQTTACL(cfg.UserAllowPub, claims),
					AllowSub:   expandUserMQTTACL(cfg.UserAllowSub, claims),
				}, true
			}
		}
		if cfg.AllowAnonymous {
			return mqtt.AuthResult{
				DeviceName: clientID,
				Username:   username,
				AllowPub:   "#",
				AllowSub:   "#",
			}, true
		}
		return mqtt.AuthResult{}, false
	}
}

func resolveTCPProject(deps *Deps, username, password string) (int, bool) {
	if deps.DB == nil || deps.ProjectDBManager == nil {
		return 0, false
	}
	if prefix, devUser, ok := strings.Cut(username, ":"); ok && prefix != "" && devUser != "" {
		var pid int
		if id, err := strconv.Atoi(prefix); err == nil && id > 0 {
			pid = id
		} else {
			pid, _ = store.LookupProjectIDBySlug(deps.DB.DB, prefix)
		}
		if pid > 0 {
			if pdb := openExistingProjectDB(deps.ProjectDBManager, pid); pdb != nil {
				if cfg, err := store.LoadMQTTConfig(pdb.Reader()); err == nil && cfg.Enabled {
					if dev, ok, err := store.LookupMQTTDevice(pdb.Reader(), devUser); err == nil && ok && checkMQTTToken(dev.SecretHash, password) {
						return pid, true
					}
					if cfg.AllowAnonymous {
						return pid, true
					}
				}
			}
		}
		return 0, false
	}

	// Fallback: scan enabled projects for a matching device username+token.
	ids, err := store.ListProjectIDs(deps.DB.DB)
	if err != nil {
		return 0, false
	}
	for _, pid := range ids {
		pdb := openExistingProjectDB(deps.ProjectDBManager, pid)
		if pdb == nil {
			continue
		}
		cfg, err := store.LoadMQTTConfig(pdb.Reader())
		if err != nil || !cfg.Enabled {
			continue
		}
		if username != "" {
			if dev, ok, err := store.LookupMQTTDevice(pdb.Reader(), username); err == nil && ok && checkMQTTToken(dev.SecretHash, password) {
				return pid, true
			}
		}
	}
	return 0, false
}

func touchDeviceAndEmitRecovered(deps *Deps, projectID int, writer *sql.DB, username string) {
	if username == "" {
		return
	}
	if devName, recovered := store.TouchMQTTDevice(writer, username); recovered && deps.Bus != nil {
		deps.Bus.Publish(events.Event{
			Type:      events.EventMQTTDeviceRecovered,
			ProjectID: projectID,
			Payload: map[string]any{
				"device":   devName,
				"username": username,
			},
		})
	}
}

func makeMQTTWatchdogTick(deps *Deps, projectID int) func() {
	return func() {
		pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
		if pdb == nil {
			return
		}
		stale, err := store.FindAndMarkStaleMQTTDevices(pdb.Writer())
		if err != nil || len(stale) == 0 || deps.Bus == nil {
			return
		}
		for _, d := range stale {
			payload := map[string]any{
				"device":        d.Name,
				"username":      d.Username,
				"heartbeat_sec": d.HeartbeatSec,
			}
			if d.LastSeenAt.Valid {
				payload["last_seen_at"] = d.LastSeenAt.Time.UTC().Format(time.RFC3339)
			}
			deps.Bus.Publish(events.Event{
				Type:      events.EventMQTTDeviceStale,
				ProjectID: projectID,
				Payload:   payload,
			})
		}
	}
}

func makeMQTTClientHook(deps *Deps, projectID int, evType events.EventType) mqtt.ClientHandler {
	return func(ev mqtt.ClientEvent) {
		if ev.Username != "" {
			if pdb := openExistingProjectDB(deps.ProjectDBManager, projectID); pdb != nil {
				touchDeviceAndEmitRecovered(deps, projectID, pdb.Writer(), ev.Username)
			}
		}
		if deps.Bus != nil {
			payload := map[string]any{
				"client_id": ev.ClientID,
				"device":    ev.DeviceName,
				"username":  ev.Username,
			}
			if evType == events.EventMQTTClientDisconnected {
				payload["clean_exit"] = ev.CleanExit
				payload["replaced"] = ev.Replaced
			}
			deps.Bus.Publish(events.Event{
				Type:      evType,
				ProjectID: projectID,
				Payload:   payload,
			})
		}
	}
}

func makeMQTTPublishHook(deps *Deps, projectID int, rules []store.MQTTRule) mqtt.PublishHandler {
	var stateMu sync.Mutex
	lastTouch := make(map[string]time.Time)
	lastInsert := make(map[string]time.Time)
	lastPrune := make(map[string]time.Time)
	return func(ev mqtt.PublishEvent) {
		pdb := openExistingProjectDB(deps.ProjectDBManager, projectID)
		if pdb == nil {
			return
		}
		now := time.Now()
		if ev.Username != "" {
			stateMu.Lock()
			prev, seen := lastTouch[ev.Username]
			shouldTouch := !seen || now.Sub(prev) >= mqttTouchThrottle
			if shouldTouch {
				lastTouch[ev.Username] = now
			}
			stateMu.Unlock()
			if shouldTouch {
				touchDeviceAndEmitRecovered(deps, projectID, pdb.Writer(), ev.Username)
			}
		}
		if deps.Bus != nil {
			deps.Bus.Publish(events.Event{
				Type:      events.EventMQTTPublish,
				ProjectID: projectID,
				Payload: map[string]any{
					"topic":     ev.Topic,
					"payload":   string(ev.Payload),
					"client_id": ev.ClientID,
					"device":    ev.DeviceName,
				},
			})
		}
		if len(rules) == 0 {
			return
		}
		var parsed any
		var parsedObj map[string]any
		if len(ev.Payload) > 0 && json.Unmarshal(ev.Payload, &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				parsedObj = m
			}
		}
		for _, rule := range rules {
			if !rule.Enabled {
				continue
			}
			matched, vars := mqtt.MatchTopicExtract(rule.TopicFilter, ev.Topic)
			if !matched {
				continue
			}
			if rule.RollupInterval != "" {
				recordMQTTRollup(pdb.Writer(), rule, ev, parsedObj, parsed, now)
			}
			if rule.TargetTable != "" && mqttIdentRE.MatchString(rule.TargetTable) && !strings.HasPrefix(strings.ToLower(rule.TargetTable), "of_") {
				allowInsert := true
				if rule.MinIntervalSec > 0 {
					key := rule.Name + "\x00" + ev.Topic
					minGap := time.Duration(rule.MinIntervalSec) * time.Second
					stateMu.Lock()
					if prev, ok := lastInsert[key]; ok && now.Sub(prev) < minGap {
						allowInsert = false
					} else {
						if len(lastInsert) >= 4096 {
							for k, ts := range lastInsert {
								if now.Sub(ts) >= minGap {
									delete(lastInsert, k)
								}
							}
							if len(lastInsert) >= 4096 {
								clear(lastInsert)
							}
						}
						lastInsert[key] = now
					}
					stateMu.Unlock()
				}
				if allowInsert {
					insertMQTTTableRow(pdb.Reader(), pdb.Writer(), deps.Encryptor, rule.TargetTable, ev, parsedObj, parsed, vars)
				}
				if rule.RetentionDays > 0 {
					stateMu.Lock()
					prev, ok := lastPrune[rule.Name]
					shouldPrune := !ok || now.Sub(prev) >= mqttPruneThrottle
					if shouldPrune {
						lastPrune[rule.Name] = now
					}
					stateMu.Unlock()
					if shouldPrune {
						pruneMQTTTable(pdb.Reader(), pdb.Writer(), rule.TargetTable, rule.RetentionDays)
					}
				}
			}
			if rule.FunctionPath != "" {
				runMQTTFunction(deps, projectID, pdb, rule.FunctionPath, ev, parsed, vars)
			}
		}
	}
}

func mqttRollupBucket(now time.Time, interval string) string {
	u := now.UTC()
	switch interval {
	case "1m":
		return u.Truncate(time.Minute).Format("2006-01-02 15:04:05")
	case "5m":
		return u.Truncate(5 * time.Minute).Format("2006-01-02 15:04:05")
	case "15m":
		return u.Truncate(15 * time.Minute).Format("2006-01-02 15:04:05")
	case "1h":
		return u.Truncate(time.Hour).Format("2006-01-02 15:04:05")
	case "1d":
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	default:
		return ""
	}
}

func recordMQTTRollup(writer *sql.DB, rule store.MQTTRule, ev mqtt.PublishEvent, obj map[string]any, parsed any, now time.Time) {
	bucket := mqttRollupBucket(now, rule.RollupInterval)
	if bucket == "" {
		return
	}
	if len(obj) > 0 {
		for k, v := range obj {
			if f, ok := v.(float64); ok && mqttIdentRE.MatchString(k) {
				store.UpsertMQTTRollup(writer, rule.Name, rule.RollupInterval, bucket, ev.Topic, k, f)
			}
		}
		return
	}
	if f, ok := parsed.(float64); ok {
		store.UpsertMQTTRollup(writer, rule.Name, rule.RollupInterval, bucket, ev.Topic, "value", f)
		return
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(string(ev.Payload)), 64); err == nil {
		store.UpsertMQTTRollup(writer, rule.Name, rule.RollupInterval, bucket, ev.Topic, "value", f)
	}
}

func pruneMQTTTable(reader, writer *sql.DB, table string, retentionDays int) {
	cols := store.TableColumnNames(reader, table)
	if len(cols) == 0 {
		return
	}
	colMap := make(map[string]string, len(cols))
	for _, c := range cols {
		colMap[strings.ToLower(c)] = c
	}
	for _, candidate := range []string{"created_at", "recorded_at", "timestamp", "ts"} {
		if canon, ok := colMap[candidate]; ok && mqttIdentRE.MatchString(canon) {
			store.PruneMQTTTableOlderThan(writer, table, canon, retentionDays)
			return
		}
	}
}

func insertMQTTTableRow(reader, writer *sql.DB, enc *security.Encryptor, table string, ev mqtt.PublishEvent, obj map[string]any, parsed any, topicVars map[string]string) {
	tableCols := store.TableColumnNames(reader, table)
	if len(tableCols) == 0 {
		return
	}
	canonCols := make(map[string]string, len(tableCols))
	for _, c := range tableCols {
		canonCols[strings.ToLower(c)] = c
	}

	row := make(map[string]any, len(obj)+len(topicVars)+4)
	for k, v := range obj {
		lk := strings.ToLower(k)
		if _, exists := row[lk]; !exists {
			row[lk] = v
		}
	}
	for k, v := range topicVars {
		lk := strings.ToLower(k)
		if _, exists := row[lk]; !exists {
			row[lk] = v
		}
	}
	if _, ok := row["topic"]; !ok {
		row["topic"] = ev.Topic
	}
	if _, ok := row["device"]; !ok {
		dev := ev.DeviceName
		if dev == "" {
			dev = ev.ClientID
		}
		row["device"] = dev
	}
	if _, ok := row["payload"]; !ok {
		row["payload"] = string(ev.Payload)
	}
	if len(obj) == 0 {
		if _, ok := row["value"]; !ok {
			if parsed != nil {
				row["value"] = parsed
			} else if f, err := strconv.ParseFloat(strings.TrimSpace(string(ev.Payload)), 64); err == nil {
				row["value"] = f
			} else {
				row["value"] = string(ev.Payload)
			}
		}
	}
	// If the table has a NOT NULL timestamp column with no DEFAULT (e.g. recorded_at TEXT NOT NULL)
	// and the payload did not include one, auto-fill the current UTC timestamp so the rule insert
	// succeeds instead of silently failing a NOT NULL constraint.
	if reqCols, err := store.RequiredColumns(reader, table); err == nil {
		nowISO := time.Now().UTC().Format(time.RFC3339)
		for _, tsCol := range []string{"recorded_at", "created_at", "timestamp", "ts"} {
			if canon, exists := canonCols[tsCol]; exists && reqCols[canon] {
				if _, set := row[tsCol]; !set {
					row[tsCol] = nowISO
				}
			}
		}
	}

	secureCols, err := store.LoadSecureColumns(reader, table)
	if err != nil {
		// Fail closed if secure-column metadata cannot be read.
		return
	}
	cols := make([]string, 0, len(row))
	placeholders := make([]string, 0, len(row))
	args := make([]any, 0, len(row))
	for lk, v := range row {
		canon, exists := canonCols[lk]
		if !exists || !mqttIdentRE.MatchString(canon) {
			continue
		}
		if kind, ok := lookupSecureCol(secureCols, canon); ok {
			processed, serr := security.ProcessSecureValue(kind, v, enc)
			if serr != nil {
				return
			}
			v = processed
		}
		cols = append(cols, `"`+canon+`"`)
		placeholders = append(placeholders, "?")
		args = append(args, v)
	}
	store.InsertMQTTRow(writer, table, cols, placeholders, args)
}

func runMQTTFunction(deps *Deps, projectID int, pdb *db.ProjectDB, fnPath string, ev mqtt.PublishEvent, parsed any, topicVars map[string]string) {
	ep, err := store.LoadFunctionEndpoint(pdb.Reader(), fnPath)
	if err != nil || ep == nil {
		return
	}
	rt := funcexec.NewRuntime(projectID, pdb.Reader(), pdb.Writer(), deps.completeDeps())
	rt.Bus = deps.Bus
	params := map[string]any{}
	for k, v := range topicVars {
		params[k] = v
	}
	payloadStr := string(ev.Payload)
	body := map[string]any{
		"topic":     ev.Topic,
		"payload":   payloadStr,
		"json":      parsed,
		"params":    params,
		"client_id": ev.ClientID,
		"device":    ev.DeviceName,
	}
	req := map[string]any{
		"method":    "MQTT",
		"path":      fnPath,
		"topic":     ev.Topic,
		"payload":   payloadStr,
		"json":      parsed,
		"params":    params,
		"client_id": ev.ClientID,
		"device":    ev.DeviceName,
		"body":      body,
		"query":     params,
		"headers":   map[string]any{},
		"user":      nil,
	}
	timeout := mqttDefaultFnBudget
	if ep.TimeoutMS > 0 {
		timeout = time.Duration(ep.TimeoutMS) * time.Millisecond
	}
	spec := funcexec.Spec{
		Code:        ep.Code,
		SecretsJSON: ep.Secrets,
		Timeout:     timeout,
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := funcexec.ExecuteContext(ctx, rt, spec, req); err != nil {
		db.RecordRuntimeError(pdb.ToolHandle(ctx), db.RuntimeErrorFunction, ep.Path, err.Error())
	}
}

// ServeMQTTRollups handles GET /api/__mqtt/rollups for querying streaming
// time-series aggregates produced by MQTT rules with rollup_interval set.
func (h *Handler) ServeMQTTRollups(w http.ResponseWriter, r *http.Request) {
	project, _ := r.Context().Value(projectContextKey).(*models.Project)
	if project == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	pdb := openExistingProjectDB(h.deps.ProjectDBManager, project.ID)
	if pdb == nil {
		http.Error(w, "project db unavailable", http.StatusNotFound)
		return
	}
	cfg, err := store.LoadMQTTConfig(pdb.Reader())
	if err != nil || !cfg.Enabled {
		http.Error(w, "MQTT is not enabled for this project", http.StatusNotFound)
		return
	}
	if !cfg.AllowAnonymous {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" || h.deps.JWTManager == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims, err := h.deps.JWTManager.Validate(tok)
		if err != nil || claims == nil || claims.ProjectID != project.ID {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var issuedAt *time.Time
		if claims.IssuedAt != nil {
			t := claims.IssuedAt.Time
			issuedAt = &t
		}
		if claims.UserID > 0 && tokenrevoke.Revoked(pdb.Reader(), project.ID, claims.UserID, issuedAt) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	rows, err := store.QueryMQTTRollups(
		pdb.Reader(),
		strings.TrimSpace(q.Get("rule")),
		strings.TrimSpace(q.Get("interval")),
		strings.TrimSpace(q.Get("series")),
		strings.TrimSpace(q.Get("metric")),
		strings.TrimSpace(q.Get("since")),
		limit,
	)
	if err != nil {
		http.Error(w, "query rollups failed", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []store.MQTTRollupRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"rollups": rows})
}

// ServeMQTT handles MQTT-over-WebSocket connections on /mqtt for the resolved project.
func (h *Handler) ServeMQTT(w http.ResponseWriter, r *http.Request) {
	if h.deps.MQTTHub == nil {
		http.Error(w, "MQTT not available", http.StatusNotFound)
		return
	}
	project, _ := r.Context().Value(projectContextKey).(*models.Project)
	if project == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	broker, err := h.deps.MQTTHub.BrokerFor(project.ID)
	if err != nil || broker == nil {
		http.Error(w, "MQTT is not enabled for this project", http.StatusNotFound)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       []string{"mqtt", "mqttv3.1"},
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	if pdb := openExistingProjectDB(h.deps.ProjectDBManager, project.ID); pdb != nil {
		pdb.Pin()
		defer pdb.Unpin()
	}
	conn := mqtt.NewWSConn(r.Context(), ws)
	broker.HandleConn(r.Context(), conn)
}

type sqlRetainedStore struct {
	mgr       *db.ProjectDBManager
	projectID int
}

func (s *sqlRetainedStore) SetRetained(topic string, payload []byte, qos byte) error {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return sql.ErrConnDone
	}
	return store.SetMQTTRetained(pdb.Writer(), topic, payload, qos)
}

func (s *sqlRetainedStore) DeleteRetained(topic string) error {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return sql.ErrConnDone
	}
	return store.DeleteMQTTRetained(pdb.Writer(), topic)
}

func (s *sqlRetainedStore) ListRetained() ([]mqtt.RetainedMessage, error) {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := store.ListMQTTRetained(pdb.Reader())
	if err != nil {
		return nil, err
	}
	out := make([]mqtt.RetainedMessage, len(rows))
	for i, r := range rows {
		out[i] = mqtt.RetainedMessage{
			Topic:   r.Topic,
			Payload: r.Payload,
			QoS:     r.QoS,
		}
	}
	return out, nil
}

type sqlSessionStore struct {
	mgr       *db.ProjectDBManager
	projectID int
}

func (s *sqlSessionStore) SaveSubscription(clientID, filter string, qos byte) error {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return sql.ErrConnDone
	}
	return store.SaveMQTTSubscription(pdb.Writer(), clientID, filter, qos)
}

func (s *sqlSessionStore) DeleteSubscription(clientID, filter string) error {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return sql.ErrConnDone
	}
	return store.DeleteMQTTSubscription(pdb.Writer(), clientID, filter)
}

func (s *sqlSessionStore) ClearSession(clientID string) error {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return sql.ErrConnDone
	}
	return store.ClearMQTTSession(pdb.Writer(), clientID)
}

func (s *sqlSessionStore) ListSubscriptions() (map[string]map[string]byte, error) {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return nil, sql.ErrConnDone
	}
	return store.ListMQTTSubscriptions(pdb.Reader())
}

func (s *sqlSessionStore) EnqueueOffline(clientID, topic string, payload []byte, qos byte, maxPerClient int) error {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return sql.ErrConnDone
	}
	return store.EnqueueMQTTOffline(pdb.Writer(), clientID, topic, payload, qos, maxPerClient)
}

func (s *sqlSessionStore) DrainOffline(clientID string, limit int) ([]mqtt.QueuedMessage, error) {
	pdb := openExistingProjectDB(s.mgr, s.projectID)
	if pdb == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := store.DrainMQTTOffline(pdb.Writer(), clientID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]mqtt.QueuedMessage, len(rows))
	for i, r := range rows {
		out[i] = mqtt.QueuedMessage{
			ID:      r.ID,
			Topic:   r.Topic,
			Payload: r.Payload,
			QoS:     r.QoS,
		}
	}
	return out, nil
}
