package public

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/internal/funcexec"
	"openfabrica/internal/mqtt"
	"openfabrica/store"
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

// NewMQTTHub creates the shared multi-project MQTT hub wired to project DBs,
// function execution, and the event bus, and registers ctx.mqtt.publish.
func NewMQTTHub(deps *Deps) *mqtt.Hub {
	hub := mqtt.NewHub(mqtt.ProjectHooks{
		Enabled: func(projectID int) bool {
			if deps.ProjectDBManager == nil {
				return false
			}
			pdb := deps.ProjectDBManager.Get(projectID)
			if pdb == nil {
				return false
			}
			cfg, err := store.LoadMQTTConfig(pdb.Reader())
			return err == nil && cfg.Enabled
		},
		LoadConfig: func(projectID int) (mqtt.BrokerConfig, error) {
			pdb := deps.ProjectDBManager.Get(projectID)
			if pdb == nil {
				return mqtt.BrokerConfig{}, sql.ErrNoRows
			}
			cfg, err := store.LoadMQTTConfig(pdb.Reader())
			if err != nil {
				return mqtt.BrokerConfig{}, err
			}
			return mqtt.BrokerConfig{
				MaxClients: cfg.MaxClients,
				Retained:   &sqlRetainedStore{pdb: pdb},
				Auth:       makeMQTTAuth(deps, projectID, pdb, cfg.AllowAnonymous),
				OnPublish:  makeMQTTPublishHook(deps, projectID, pdb),
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
	return hub
}

func makeMQTTAuth(deps *Deps, projectID int, pdb *db.ProjectDB, allowAnon bool) mqtt.Authenticator {
	return func(clientID, username, password string) (mqtt.AuthResult, bool) {
		// Strip optional "<project>:" prefix when routed from raw TCP.
		if _, rest, ok := strings.Cut(username, ":"); ok {
			username = rest
		}
		if username != "" {
			if dev, ok, err := store.LookupMQTTDevice(pdb.Reader(), username); err == nil && ok {
				if checkMQTTToken(dev.SecretHash, password) {
					store.TouchMQTTDevice(pdb.Writer(), dev.Username)
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
				ident := strconv.Itoa(claims.UserID)
				return mqtt.AuthResult{
					DeviceName: ident,
					Username:   ident,
					AllowPub:   "#",
					AllowSub:   "#",
				}, true
			}
		}
		if allowAnon {
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
			if pdb := deps.ProjectDBManager.Get(pid); pdb != nil {
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
		pdb := deps.ProjectDBManager.Get(pid)
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

func makeMQTTPublishHook(deps *Deps, projectID int, pdb *db.ProjectDB) mqtt.PublishHandler {
	return func(ev mqtt.PublishEvent) {
		if deps.Bus != nil {
			deps.Bus.Publish(events.Event{
				Type:      "mqtt.publish",
				ProjectID: projectID,
				Payload: map[string]any{
					"topic":     ev.Topic,
					"payload":   string(ev.Payload),
					"client_id": ev.ClientID,
					"device":    ev.DeviceName,
				},
			})
		}
		rules, err := store.ListMQTTRules(pdb.Reader())
		if err != nil || len(rules) == 0 {
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
			if !rule.Enabled || !mqtt.MatchTopic(rule.TopicFilter, ev.Topic) {
				continue
			}
			if rule.TargetTable != "" && mqttIdentRE.MatchString(rule.TargetTable) && !strings.HasPrefix(strings.ToLower(rule.TargetTable), "of_") {
				insertMQTTTableRow(pdb.Writer(), rule.TargetTable, ev, parsedObj)
			}
			if rule.FunctionPath != "" {
				runMQTTFunction(deps, projectID, pdb, rule.FunctionPath, ev, parsed)
			}
		}
	}
}

func insertMQTTTableRow(writer *sql.DB, table string, ev mqtt.PublishEvent, obj map[string]any) {
	if len(obj) == 0 {
		obj = map[string]any{
			"topic":   ev.Topic,
			"payload": string(ev.Payload),
			"device":  ev.DeviceName,
		}
	}
	cols := make([]string, 0, len(obj))
	placeholders := make([]string, 0, len(obj))
	args := make([]any, 0, len(obj))
	for k, v := range obj {
		if !mqttIdentRE.MatchString(k) {
			continue
		}
		cols = append(cols, `"`+k+`"`)
		placeholders = append(placeholders, "?")
		args = append(args, v)
	}
	store.InsertMQTTRow(writer, table, cols, placeholders, args)
}

func runMQTTFunction(deps *Deps, projectID int, pdb *db.ProjectDB, fnPath string, ev mqtt.PublishEvent, parsed any) {
	ep, err := store.LoadFunctionEndpoint(pdb.Reader(), fnPath)
	if err != nil || ep == nil {
		return
	}
	rt := funcexec.NewRuntime(projectID, pdb.Reader(), pdb.Writer(), deps.completeDeps())
	req := map[string]any{
		"method":    "MQTT",
		"path":      fnPath,
		"topic":     ev.Topic,
		"payload":   string(ev.Payload),
		"json":      parsed,
		"client_id": ev.ClientID,
		"device":    ev.DeviceName,
	}
	_, _ = funcexec.Execute(rt, funcexec.Spec{
		Code:        ep.Code,
		SecretsJSON: ep.Secrets,
	}, req)
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
	conn := mqtt.NewWSConn(r.Context(), ws)
	broker.HandleConn(r.Context(), conn)
}

type sqlRetainedStore struct {
	pdb *db.ProjectDB
}

func (s *sqlRetainedStore) SetRetained(topic string, payload []byte, qos byte) error {
	return store.SetMQTTRetained(s.pdb.Writer(), topic, payload, qos)
}

func (s *sqlRetainedStore) DeleteRetained(topic string) error {
	return store.DeleteMQTTRetained(s.pdb.Writer(), topic)
}

func (s *sqlRetainedStore) ListRetained() ([]mqtt.RetainedMessage, error) {
	rows, err := store.ListMQTTRetained(s.pdb.Reader())
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
