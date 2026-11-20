package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"openfabrica/events"
	"openfabrica/internal/funcexec"
	"openfabrica/internal/mqtt"
	"openfabrica/store"
)

type ProjectMQTTHandler struct {
	deps *Deps
}

func (h *ProjectMQTTHandler) emitChanged(projectID int) {
	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.Event{
			Type:      events.EventMQTTConfigChanged,
			ProjectID: projectID,
		})
	}
}

func hashAdminMQTTToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateAdminMQTTToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ofmqtt_" + hex.EncodeToString(b[:]), nil
}

func validAdminUserACL(acl string) bool {
	for _, raw := range strings.Split(acl, ",") {
		f := strings.TrimSpace(raw)
		if f == "" || strings.EqualFold(f, "none") {
			continue
		}
		probe := strings.ReplaceAll(strings.ReplaceAll(f, "{user_id}", "1"), "{tenant_id}", "1")
		if mqtt.ValidateTopicFilter(probe) != nil {
			return false
		}
	}
	return true
}

func (h *ProjectMQTTHandler) Status(w http.ResponseWriter, r *http.Request) {
	projectID, pdb := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if pdb == nil {
		return
	}
	cfg, err := store.LoadMQTTConfig(pdb.Reader())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load mqtt config")
		return
	}
	devices, err := store.ListMQTTDevices(pdb.Reader())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list mqtt devices")
		return
	}
	rules, err := store.ListMQTTRules(pdb.Reader())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list mqtt rules")
		return
	}
	bridges, err := store.ListMQTTBridges(pdb.Reader())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list mqtt bridges")
		return
	}
	retained, err := store.ListMQTTRetained(pdb.Reader())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list mqtt retained")
		return
	}

	devViews := make([]map[string]any, len(devices))
	for i, d := range devices {
		v := map[string]any{
			"name":          d.Name,
			"username":      d.Username,
			"allow_pub":     d.AllowPub,
			"allow_sub":     d.AllowSub,
			"heartbeat_sec": d.HeartbeatSec,
			"stale":         d.StaleNotified,
			"enabled":       d.Enabled,
		}
		if d.LastSeenAt.Valid {
			v["last_seen_at"] = d.LastSeenAt.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		devViews[i] = v
	}

	ruleViews := make([]map[string]any, len(rules))
	for i, rl := range rules {
		ruleViews[i] = map[string]any{
			"name":             rl.Name,
			"topic_filter":     rl.TopicFilter,
			"target_table":     rl.TargetTable,
			"function_path":    rl.FunctionPath,
			"min_interval_sec": rl.MinIntervalSec,
			"retention_days":   rl.RetentionDays,
			"rollup_interval":  rl.RollupInterval,
			"enabled":          rl.Enabled,
		}
	}

	bridgeViews := make([]map[string]any, len(bridges))
	for i, br := range bridges {
		bridgeViews[i] = map[string]any{
			"name":          br.Name,
			"broker_url":    br.BrokerURL,
			"client_id":     br.ClientID,
			"username":      br.Username,
			"remote_sub":    br.RemoteSub,
			"local_prefix":  br.LocalPrefix,
			"local_pub":     br.LocalPub,
			"remote_prefix": br.RemotePrefix,
			"qos":           br.QoS,
			"enabled":       br.Enabled,
		}
	}

	retViews := make([]map[string]any, len(retained))
	for i, rm := range retained {
		retViews[i] = map[string]any{
			"topic":      rm.Topic,
			"payload":    string(rm.Payload),
			"qos":        rm.QoS,
			"updated_at": rm.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
	}

	tcpPort := 0
	if h.deps.Config != nil {
		tcpPort = h.deps.Config.MQTTPort
	}
	out := map[string]any{
		"config": map[string]any{
			"enabled":         cfg.Enabled,
			"allow_anonymous": cfg.AllowAnonymous,
			"max_clients":     cfg.MaxClients,
			"user_allow_pub":  cfg.UserAllowPub,
			"user_allow_sub":  cfg.UserAllowSub,
			"ws_path":         "/mqtt",
			"tcp_port":        tcpPort,
		},
		"connected_clients": 0,
		"subscriptions":     0,
		"clients":           []any{},
		"bridge_status":     []any{},
		"devices":           devViews,
		"rules":             ruleViews,
		"bridges":           bridgeViews,
		"retained":          retViews,
	}
	if stats, ok := funcexec.MQTTStats(projectID); ok {
		out["connected_clients"] = stats["connected_clients"]
		out["clients"] = stats["clients"]
		out["subscriptions"] = stats["subscriptions"]
		if bs, exists := stats["bridges"]; exists && bs != nil {
			out["bridge_status"] = bs
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *ProjectMQTTHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	projectID, pdb := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if pdb == nil {
		return
	}
	var req struct {
		Enabled        bool   `json:"enabled"`
		AllowAnonymous bool   `json:"allow_anonymous"`
		MaxClients     int    `json:"max_clients"`
		UserAllowPub   string `json:"user_allow_pub"`
		UserAllowSub   string `json:"user_allow_sub"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	userPub := strings.TrimSpace(req.UserAllowPub)
	if userPub != "" && !validAdminUserACL(userPub) {
		writeError(w, http.StatusBadRequest, "invalid allow_pub filter")
		return
	}
	userSub := strings.TrimSpace(req.UserAllowSub)
	if userSub != "" && !validAdminUserACL(userSub) {
		writeError(w, http.StatusBadRequest, "invalid allow_sub filter")
		return
	}
	cfg := store.MQTTConfig{
		Enabled:        req.Enabled,
		AllowAnonymous: req.AllowAnonymous,
		MaxClients:     req.MaxClients,
		UserAllowPub:   userPub,
		UserAllowSub:   userSub,
	}
	if err := store.SaveMQTTConfig(pdb.Writer(), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save mqtt config")
		return
	}
	h.emitChanged(projectID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *ProjectMQTTHandler) AddDevice(w http.ResponseWriter, r *http.Request) {
	projectID, pdb := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if pdb == nil {
		return
	}
	var req struct {
		Name         string `json:"name"`
		Username     string `json:"username"`
		Token        string `json:"token"`
		AllowPub     string `json:"allow_pub"`
		AllowSub     string `json:"allow_sub"`
		HeartbeatSec int    `json:"heartbeat_sec"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	user := strings.TrimSpace(req.Username)
	if name == "" || user == "" {
		writeError(w, http.StatusBadRequest, "name and username are required")
		return
	}
	allowPub := strings.TrimSpace(req.AllowPub)
	if allowPub == "" {
		allowPub = "#"
	}
	allowSub := strings.TrimSpace(req.AllowSub)
	if allowSub == "" {
		allowSub = "#"
	}
	for _, f := range strings.Split(allowPub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				writeError(w, http.StatusBadRequest, "invalid allow_pub filter")
				return
			}
		}
	}
	for _, f := range strings.Split(allowSub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				writeError(w, http.StatusBadRequest, "invalid allow_sub filter")
				return
			}
		}
	}
	tok := strings.TrimSpace(req.Token)
	if tok == "" {
		var err error
		tok, err = generateAdminMQTTToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to generate token")
			return
		}
	}
	if err := store.UpsertMQTTDevice(pdb.Writer(), name, user, hashAdminMQTTToken(tok), allowPub, allowSub, req.HeartbeatSec); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save mqtt device")
		return
	}
	h.emitChanged(projectID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":          name,
		"username":      user,
		"token":         tok,
		"allow_pub":     allowPub,
		"allow_sub":     allowSub,
		"heartbeat_sec": req.HeartbeatSec,
	})
}

func (h *ProjectMQTTHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	projectID, pdb := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if pdb == nil {
		return
	}
	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "device name is required")
		return
	}
	deleted, err := store.DeleteMQTTDevice(pdb.Writer(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete device")
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	h.emitChanged(projectID)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func (h *ProjectMQTTHandler) AddBridge(w http.ResponseWriter, r *http.Request) {
	projectID, pdb := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if pdb == nil {
		return
	}
	var req struct {
		Name         string `json:"name"`
		BrokerURL    string `json:"broker_url"`
		RemoteSub    string `json:"remote_sub"`
		LocalPrefix  string `json:"local_prefix"`
		LocalPub     string `json:"local_pub"`
		RemotePrefix string `json:"remote_prefix"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		ClientID     string `json:"client_id"`
		QoS          int    `json:"qos"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	brokerURL := strings.TrimSpace(req.BrokerURL)
	if name == "" || brokerURL == "" {
		writeError(w, http.StatusBadRequest, "name and broker_url are required")
		return
	}
	if _, _, _, err := mqtt.ValidateBridgeURL(brokerURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid broker_url")
		return
	}
	remoteSub := strings.TrimSpace(req.RemoteSub)
	localPub := strings.TrimSpace(req.LocalPub)
	if remoteSub == "" && localPub == "" {
		writeError(w, http.StatusBadRequest, "at least one of remote_sub or local_pub is required")
		return
	}
	for _, f := range strings.Split(remoteSub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				writeError(w, http.StatusBadRequest, "invalid remote_sub filter")
				return
			}
		}
	}
	for _, f := range strings.Split(localPub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				writeError(w, http.StatusBadRequest, "invalid local_pub filter")
				return
			}
		}
	}
	var passwordEnc string
	if req.Password != "" {
		if h.deps.Encryptor != nil {
			sealed, err := h.deps.Encryptor.SealOptional(req.Password)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to encrypt bridge password")
				return
			}
			passwordEnc = sealed
		} else {
			passwordEnc = req.Password
		}
	}
	qos := byte(0)
	if req.QoS > 0 {
		qos = 1
	}
	br := store.MQTTBridge{
		Name:         name,
		BrokerURL:    brokerURL,
		ClientID:     strings.TrimSpace(req.ClientID),
		Username:     strings.TrimSpace(req.Username),
		PasswordEnc:  passwordEnc,
		RemoteSub:    remoteSub,
		LocalPrefix:  strings.TrimSpace(req.LocalPrefix),
		LocalPub:     localPub,
		RemotePrefix: strings.TrimSpace(req.RemotePrefix),
		QoS:          qos,
	}
	if err := store.UpsertMQTTBridge(pdb.Writer(), br); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save mqtt bridge")
		return
	}
	h.emitChanged(projectID)
	writeJSON(w, http.StatusCreated, map[string]any{"name": br.Name, "broker_url": br.BrokerURL})
}

func (h *ProjectMQTTHandler) DeleteBridge(w http.ResponseWriter, r *http.Request) {
	projectID, pdb := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if pdb == nil {
		return
	}
	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "bridge name is required")
		return
	}
	deleted, err := store.DeleteMQTTBridge(pdb.Writer(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete bridge")
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}
	h.emitChanged(projectID)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func (h *ProjectMQTTHandler) Publish(w http.ResponseWriter, r *http.Request) {
	projectID, pdb := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if pdb == nil {
		return
	}
	var req struct {
		Topic   string `json:"topic"`
		Payload string `json:"payload"`
		QoS     int    `json:"qos"`
		Retain  bool   `json:"retain"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	topic := strings.TrimSpace(req.Topic)
	if err := mqtt.ValidatePublishTopic(topic); err != nil {
		writeError(w, http.StatusBadRequest, "invalid mqtt topic")
		return
	}
	qos := byte(0)
	if req.QoS > 0 {
		qos = 1
	}
	if req.Retain {
		if req.Payload == "" {
			_ = store.DeleteMQTTRetained(pdb.Writer(), topic)
		} else {
			_ = store.SetMQTTRetained(pdb.Writer(), topic, []byte(req.Payload), qos)
		}
	}
	if err := funcexec.PublishMQTT(projectID, topic, []byte(req.Payload), qos, req.Retain); err != nil {
		writeError(w, http.StatusBadRequest, "mqtt broker is not enabled for this project")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "topic": topic})
}
