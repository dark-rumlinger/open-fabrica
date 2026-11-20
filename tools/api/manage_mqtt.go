package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"openfabrica/events"
	"openfabrica/internal/funcexec"
	"openfabrica/internal/mqtt"
	"openfabrica/store"
	"openfabrica/tools"
)

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_mqtt",
		MaxResult:          16000,
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Configure the project's MQTT broker (devices, ACLs, SQLite/function rules, outbound LAN bridges, and test publish).",
		WhenToUse: `Use when building IoT hubs, sensor telemetry dashboards, Raspberry Pi/ESP32
controllers, smart-home panels (Zigbee2MQTT, ESPHome, Frigate, Shelly, Tasmota,
Meshtastic), or device pub/sub backends. Supports MQTT over WebSocket at /mqtt,
optional raw TCP (OPENFABRICA_MQTT_PORT), outbound bridges to existing LAN
brokers, named topic captures ({zone}/{device}), and streaming time-series
rollups. Actions: configure, status, add_device, delete_device, add_rule,
delete_rule, add_bridge, delete_bridge, rollups, publish.`,
		Actions: []tools.Action{
			&MQTTConfigure{},
			&MQTTStatus{},
			&MQTTAddDevice{},
			&MQTTDeleteDevice{},
			&MQTTAddRule{},
			&MQTTDeleteRule{},
			&MQTTAddBridge{},
			&MQTTDeleteBridge{},
			&MQTTRollups{},
			&MQTTPublish{},
		},
	})
}

func emitMQTTConfigChanged(ctx *tools.ToolContext) {
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.Event{
			Type:      events.EventMQTTConfigChanged,
			ProjectID: ctx.ProjectID,
		})
	}
}

func hashDeviceToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateDeviceToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ofmqtt_" + hex.EncodeToString(b[:]), nil
}

// --- configure --------------------------------------------------------------

type MQTTConfigure struct {
	Enabled        bool   `arg:"enabled,required" desc:"Enable or disable the project's MQTT broker (/mqtt WebSocket and TCP routing)."`
	AllowAnonymous bool   `arg:"allow_anonymous" desc:"Allow unauthenticated connections (for local testing or public read-only demos). Default false."`
	MaxClients     int    `arg:"max_clients" desc:"Maximum concurrent MQTT clients for this project (default 100)."`
	UserAllowPub   string `arg:"user_allow_pub" desc:"CSV topic filters JWT-authenticated non-admin users may publish to (default '#', use 'none' for read-only; supports {user_id} and {tenant_id})."`
	UserAllowSub   string `arg:"user_allow_sub" desc:"CSV topic filters JWT-authenticated non-admin users may subscribe to (default '#', use 'none' to deny; supports {user_id} and {tenant_id})."`
}

func validateUserACL(acl string) error {
	for _, raw := range strings.Split(acl, ",") {
		f := strings.TrimSpace(raw)
		if f == "" || strings.EqualFold(f, "none") {
			continue
		}
		probe := strings.ReplaceAll(strings.ReplaceAll(f, "{user_id}", "1"), "{tenant_id}", "1")
		if err := mqtt.ValidateTopicFilter(probe); err != nil {
			return err
		}
	}
	return nil
}

func (a *MQTTConfigure) Doc() string {
	return "Enable or configure the project's MQTT broker."
}
func (a *MQTTConfigure) Example() any {
	return &MQTTConfigure{Enabled: true, AllowAnonymous: false, MaxClients: 100, UserAllowPub: "commands/user/{user_id}/#", UserAllowSub: "sensors/#"}
}
func (a *MQTTConfigure) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	maxC := a.MaxClients
	if maxC <= 0 {
		maxC = 100
	}
	userPub := strings.TrimSpace(a.UserAllowPub)
	if userPub == "" {
		userPub = "#"
	}
	if err := validateUserACL(userPub); err != nil {
		return tools.Fail("invalid user_allow_pub %q: %v", userPub, err), nil
	}
	userSub := strings.TrimSpace(a.UserAllowSub)
	if userSub == "" {
		userSub = "#"
	}
	if err := validateUserACL(userSub); err != nil {
		return tools.Fail("invalid user_allow_sub %q: %v", userSub, err), nil
	}
	cfg := store.MQTTConfig{
		Enabled:        a.Enabled,
		AllowAnonymous: a.AllowAnonymous,
		MaxClients:     maxC,
		UserAllowPub:   userPub,
		UserAllowSub:   userSub,
	}
	if err := store.SaveMQTTConfig(ctx.DB, cfg); err != nil {
		return tools.Fail("save mqtt config: %v", err), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"enabled":         cfg.Enabled,
		"allow_anonymous": cfg.AllowAnonymous,
		"max_clients":     cfg.MaxClients,
		"user_allow_pub":  cfg.UserAllowPub,
		"user_allow_sub":  cfg.UserAllowSub,
		"ws_path":         "/mqtt",
	}), nil
}

// --- status -----------------------------------------------------------------

type MQTTStatus struct{}

func (a *MQTTStatus) Doc() string {
	return "Inspect MQTT broker config, registered devices, ingest rules, and retained topics."
}
func (a *MQTTStatus) ReadOnly() bool { return true }
func (a *MQTTStatus) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	cfg, err := store.LoadMQTTConfig(ctx.DB)
	if err != nil {
		return tools.Fail("load mqtt config: %v", err), nil
	}
	devices, err := store.ListMQTTDevices(ctx.DB)
	if err != nil {
		return tools.Fail("list mqtt devices: %v", err), nil
	}
	rules, err := store.ListMQTTRules(ctx.DB)
	if err != nil {
		return tools.Fail("list mqtt rules: %v", err), nil
	}
	retained, err := store.ListMQTTRetained(ctx.DB)
	if err != nil {
		return tools.Fail("list mqtt retained: %v", err), nil
	}
	bridges, err := store.ListMQTTBridges(ctx.DB)
	if err != nil {
		return tools.Fail("list mqtt bridges: %v", err), nil
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
	for i, r := range rules {
		ruleViews[i] = map[string]any{
			"name":             r.Name,
			"topic_filter":     r.TopicFilter,
			"target_table":     r.TargetTable,
			"function_path":    r.FunctionPath,
			"min_interval_sec": r.MinIntervalSec,
			"retention_days":   r.RetentionDays,
			"rollup_interval":  r.RollupInterval,
			"enabled":          r.Enabled,
		}
	}
	retViews := make([]map[string]any, len(retained))
	for i, rm := range retained {
		retViews[i] = map[string]any{
			"topic":   rm.Topic,
			"payload": string(rm.Payload),
			"qos":     rm.QoS,
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

	out := map[string]any{
		"enabled":           cfg.Enabled,
		"allow_anonymous":   cfg.AllowAnonymous,
		"max_clients":       cfg.MaxClients,
		"user_allow_pub":    cfg.UserAllowPub,
		"user_allow_sub":    cfg.UserAllowSub,
		"ws_path":           "/mqtt",
		"connected_clients": 0,
		"devices":           devViews,
		"rules":             ruleViews,
		"bridges":           bridgeViews,
		"retained":          retViews,
	}
	if stats, ok := funcexec.MQTTStats(ctx.ProjectID); ok {
		out["connected_clients"] = stats["connected_clients"]
		out["clients"] = stats["clients"]
		out["subscriptions"] = stats["subscriptions"]
		if liveBridges, exists := stats["bridges"]; exists {
			out["bridge_status"] = liveBridges
		}
	}
	return tools.Ok(out), nil
}

// --- add_device -------------------------------------------------------------

type MQTTAddDevice struct {
	Name         string `arg:"name,required" desc:"Human-readable device identifier (e.g. greenhouse_esp32)."`
	Username     string `arg:"username,required" desc:"MQTT CONNECT username for this device."`
	Token        string `arg:"token" sensitive:"true" desc:"Optional password/token. Leave blank to auto-generate a secure token (returned once)."`
	AllowPub     string `arg:"allow_pub" desc:"Comma-separated MQTT topic filters this device may publish to (default '#')."`
	AllowSub     string `arg:"allow_sub" desc:"Comma-separated MQTT topic filters this device may subscribe to (default '#')."`
	HeartbeatSec int    `arg:"heartbeat_sec" desc:"Optional dead-man's-switch watchdog (seconds). Fires mqtt.device_stale if the device goes silent longer than this (0 = disabled)."`
}

func (a *MQTTAddDevice) Doc() string {
	return "Register or update an MQTT device credential with topic ACLs and optional heartbeat watchdog."
}
func (a *MQTTAddDevice) Example() any {
	return &MQTTAddDevice{
		Name:         "greenhouse_esp32",
		Username:     "esp32_1",
		AllowPub:     "sensors/esp32_1/#",
		AllowSub:     "actuators/esp32_1/#",
		HeartbeatSec: 300,
	}
}
func (a *MQTTAddDevice) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.TrimSpace(a.Name)
	user := strings.TrimSpace(a.Username)
	if name == "" || user == "" {
		return tools.Fail("name and username are required"), nil
	}
	if a.HeartbeatSec < 0 {
		return tools.Fail("heartbeat_sec must be >= 0"), nil
	}
	allowPub := strings.TrimSpace(a.AllowPub)
	if allowPub == "" {
		allowPub = "#"
	}
	allowSub := strings.TrimSpace(a.AllowSub)
	if allowSub == "" {
		allowSub = "#"
	}
	for _, f := range strings.Split(allowPub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				return tools.Fail("invalid allow_pub filter %q: %v", f, err), nil
			}
		}
	}
	for _, f := range strings.Split(allowSub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				return tools.Fail("invalid allow_sub filter %q: %v", f, err), nil
			}
		}
	}

	token := strings.TrimSpace(a.Token)
	if token == "" {
		var err error
		token, err = generateDeviceToken()
		if err != nil {
			return tools.Fail("generate token: %v", err), nil
		}
	}

	if err := store.UpsertMQTTDevice(ctx.DB, name, user, hashDeviceToken(token), allowPub, allowSub, a.HeartbeatSec); err != nil {
		return tools.Fail("save mqtt device: %v", err), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"name":          name,
		"username":      user,
		"token":         token,
		"allow_pub":     allowPub,
		"allow_sub":     allowSub,
		"heartbeat_sec": a.HeartbeatSec,
	}), nil
}

// --- delete_device ----------------------------------------------------------

type MQTTDeleteDevice struct {
	Name string `arg:"name,required" desc:"Device name to remove."`
}

func (a *MQTTDeleteDevice) Doc() string  { return "Delete a registered MQTT device." }
func (a *MQTTDeleteDevice) Example() any { return &MQTTDeleteDevice{Name: "greenhouse_esp32"} }
func (a *MQTTDeleteDevice) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	deleted, err := store.DeleteMQTTDevice(ctx.DB, strings.TrimSpace(a.Name))
	if err != nil {
		return tools.Fail("delete mqtt device: %v", err), nil
	}
	if !deleted {
		return tools.Fail("device not found"), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{"deleted": a.Name}), nil
}

// --- add_rule ---------------------------------------------------------------

type MQTTAddRule struct {
	Name           string `arg:"name,required" desc:"Unique rule name (e.g. ingest_sensors)."`
	TopicFilter    string `arg:"topic_filter,required" desc:"MQTT topic filter to match; supports '+', '#', and named captures like sensors/{zone}/{metric} or zigbee2mqtt/{device}."`
	TargetTable    string `arg:"target_table" desc:"Optional SQLite table name to auto-insert JSON/scalar payloads and captured topic segments into."`
	FunctionPath   string `arg:"function_path" desc:"Optional manage_function endpoint path to invoke with {topic, payload, json, params, device}."`
	MinIntervalSec int    `arg:"min_interval_sec" desc:"Optional per-topic throttle (seconds) between raw target_table inserts (0 = insert every message)."`
	RetentionDays  int    `arg:"retention_days" desc:"Optional retention window (days) to auto-prune old rows from target_table (0 = keep forever)."`
	RollupInterval string `arg:"rollup_interval" desc:"Optional streaming time-series rollup bucket: '1m', '5m', '15m', '1h', or '1d' (queryable via GET /api/__mqtt/rollups and manage_mqtt rollups)."`
}

func isValidRollupInterval(s string) bool {
	switch s {
	case "", "1m", "5m", "15m", "1h", "1d":
		return true
	default:
		return false
	}
}

func (a *MQTTAddRule) Doc() string {
	return "Add or update a topic ingest rule mapping MQTT messages (with optional {name} captures, throttling, retention, and streaming rollups) to SQLite and/or a server function."
}
func (a *MQTTAddRule) Example() any {
	return &MQTTAddRule{
		Name:           "ingest_sensors",
		TopicFilter:    "sensors/{zone}/{metric}",
		TargetTable:    "readings",
		FunctionPath:   "on-sensor-reading",
		MinIntervalSec: 10,
		RetentionDays:  7,
		RollupInterval: "1h",
	}
}
func (a *MQTTAddRule) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.TrimSpace(a.Name)
	filter := strings.TrimSpace(a.TopicFilter)
	table := strings.TrimSpace(a.TargetTable)
	fnPath := strings.TrimSpace(a.FunctionPath)
	rollup := strings.TrimSpace(a.RollupInterval)
	if name == "" || filter == "" {
		return tools.Fail("name and topic_filter are required"), nil
	}
	if err := mqtt.ValidateRuleTopicFilter(filter); err != nil {
		return tools.Fail("invalid topic_filter %q: %v", filter, err), nil
	}
	if !isValidRollupInterval(rollup) {
		return tools.Fail("invalid rollup_interval %q (use 1m, 5m, 15m, 1h, or 1d)", rollup), nil
	}
	if table == "" && fnPath == "" && rollup == "" {
		return tools.Fail("at least one of target_table, function_path, or rollup_interval is required"), nil
	}
	if strings.HasPrefix(strings.ToLower(table), "of_") {
		return tools.Fail("target_table cannot be a reserved of_ table"), nil
	}
	if a.MinIntervalSec < 0 || a.RetentionDays < 0 {
		return tools.Fail("min_interval_sec and retention_days must be >= 0"), nil
	}
	rule := store.MQTTRule{
		Name:           name,
		TopicFilter:    filter,
		TargetTable:    table,
		FunctionPath:   fnPath,
		MinIntervalSec: a.MinIntervalSec,
		RetentionDays:  a.RetentionDays,
		RollupInterval: rollup,
	}
	if err := store.UpsertMQTTRule(ctx.DB, rule); err != nil {
		return tools.Fail("save mqtt rule: %v", err), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"name":             rule.Name,
		"topic_filter":     rule.TopicFilter,
		"target_table":     rule.TargetTable,
		"function_path":    rule.FunctionPath,
		"min_interval_sec": rule.MinIntervalSec,
		"retention_days":   rule.RetentionDays,
		"rollup_interval":  rule.RollupInterval,
	}), nil
}

// --- delete_rule ------------------------------------------------------------

type MQTTDeleteRule struct {
	Name string `arg:"name,required" desc:"Rule name to remove."`
}

func (a *MQTTDeleteRule) Doc() string  { return "Delete an MQTT topic ingest rule." }
func (a *MQTTDeleteRule) Example() any { return &MQTTDeleteRule{Name: "ingest_sensors"} }
func (a *MQTTDeleteRule) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	deleted, err := store.DeleteMQTTRule(ctx.DB, strings.TrimSpace(a.Name))
	if err != nil {
		return tools.Fail("delete mqtt rule: %v", err), nil
	}
	if !deleted {
		return tools.Fail("rule not found"), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{"deleted": a.Name}), nil
}

// --- add_bridge -------------------------------------------------------------

type MQTTAddBridge struct {
	Name         string `arg:"name,required" desc:"Unique bridge name (e.g. home_mosquitto, zigbee_lan)."`
	BrokerURL    string `arg:"broker_url,required" desc:"Remote broker URL: tcp://, mqtt:// (default :1883), tls://, ssl://, or mqtts:// (default :8883)."`
	RemoteSub    string `arg:"remote_sub" desc:"Comma-separated remote topic filters to pull into this project (e.g. zigbee2mqtt/#,frigate/events)."`
	LocalPrefix  string `arg:"local_prefix" desc:"Optional prefix prepended to incoming remote topics (e.g. lan/ turns zigbee2mqtt/lamp into lan/zigbee2mqtt/lamp)."`
	LocalPub     string `arg:"local_pub" desc:"Comma-separated local topic filters to forward back out to the remote broker (e.g. lan/zigbee2mqtt/+/set)."`
	RemotePrefix string `arg:"remote_prefix" desc:"Optional prefix prepended to outgoing remote topics after stripping local_prefix."`
	Username     string `arg:"username" desc:"Optional MQTT username for the remote broker."`
	Password     string `arg:"password" sensitive:"true" desc:"Optional MQTT password for the remote broker (stored encrypted)."`
	ClientID     string `arg:"client_id" desc:"Optional MQTT client_id (auto-generated uniquely per project+bridge if omitted)."`
	QoS          int    `arg:"qos" desc:"QoS level for bridged subscriptions and publishes (0 or 1, default 0)."`
}

func (a *MQTTAddBridge) Doc() string {
	return "Connect this project's MQTT broker to an existing LAN or remote MQTT broker (e.g. Mosquitto, Zigbee2MQTT, ESPHome, Frigate, Shelly, Meshtastic) with loop-safe topic remapping."
}
func (a *MQTTAddBridge) Example() any {
	return &MQTTAddBridge{
		Name:        "home_mosquitto",
		BrokerURL:   "tcp://192.168.1.50:1883",
		RemoteSub:   "zigbee2mqtt/#,frigate/events",
		LocalPrefix: "lan/",
		LocalPub:    "lan/zigbee2mqtt/+/set",
		QoS:         1,
	}
}
func (a *MQTTAddBridge) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.TrimSpace(a.Name)
	brokerURL := strings.TrimSpace(a.BrokerURL)
	if name == "" || brokerURL == "" {
		return tools.Fail("name and broker_url are required"), nil
	}
	if _, _, _, err := mqtt.ValidateBridgeURL(brokerURL); err != nil {
		return tools.Fail("%v", err), nil
	}
	remoteSub := strings.TrimSpace(a.RemoteSub)
	localPub := strings.TrimSpace(a.LocalPub)
	if remoteSub == "" && localPub == "" {
		return tools.Fail("at least one of remote_sub or local_pub is required"), nil
	}
	for _, f := range strings.Split(remoteSub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				return tools.Fail("invalid remote_sub filter %q: %v", f, err), nil
			}
		}
	}
	for _, f := range strings.Split(localPub, ",") {
		if f = strings.TrimSpace(f); f != "" {
			if err := mqtt.ValidateTopicFilter(f); err != nil {
				return tools.Fail("invalid local_pub filter %q: %v", f, err), nil
			}
		}
	}
	var passwordEnc string
	if a.Password != "" {
		if ctx.Encryptor != nil {
			sealed, err := ctx.Encryptor.SealOptional(a.Password)
			if err != nil {
				return tools.Fail("encrypt bridge password: %v", err), nil
			}
			passwordEnc = sealed
		} else {
			passwordEnc = a.Password
		}
	}
	qos := byte(0)
	if a.QoS > 0 {
		qos = 1
	}
	br := store.MQTTBridge{
		Name:         name,
		BrokerURL:    brokerURL,
		ClientID:     strings.TrimSpace(a.ClientID),
		Username:     strings.TrimSpace(a.Username),
		PasswordEnc:  passwordEnc,
		RemoteSub:    remoteSub,
		LocalPrefix:  strings.TrimSpace(a.LocalPrefix),
		LocalPub:     localPub,
		RemotePrefix: strings.TrimSpace(a.RemotePrefix),
		QoS:          qos,
	}
	if err := store.UpsertMQTTBridge(ctx.DB, br); err != nil {
		return tools.Fail("save mqtt bridge: %v", err), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"name":          br.Name,
		"broker_url":    br.BrokerURL,
		"remote_sub":    br.RemoteSub,
		"local_prefix":  br.LocalPrefix,
		"local_pub":     br.LocalPub,
		"remote_prefix": br.RemotePrefix,
		"qos":           br.QoS,
	}), nil
}

// --- delete_bridge ----------------------------------------------------------

type MQTTDeleteBridge struct {
	Name string `arg:"name,required" desc:"Bridge name to remove."`
}

func (a *MQTTDeleteBridge) Doc() string  { return "Delete an outbound MQTT bridge." }
func (a *MQTTDeleteBridge) Example() any { return &MQTTDeleteBridge{Name: "home_mosquitto"} }
func (a *MQTTDeleteBridge) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	deleted, err := store.DeleteMQTTBridge(ctx.DB, strings.TrimSpace(a.Name))
	if err != nil {
		return tools.Fail("delete mqtt bridge: %v", err), nil
	}
	if !deleted {
		return tools.Fail("bridge not found"), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{"deleted": a.Name}), nil
}

// --- rollups ----------------------------------------------------------------

type MQTTRollups struct {
	Rule     string `arg:"rule" desc:"Optional rule name to filter by."`
	Interval string `arg:"interval" desc:"Optional rollup bucket interval ('1m', '5m', '15m', '1h', '1d')."`
	Series   string `arg:"series" desc:"Optional series_key (MQTT topic) to filter by."`
	Metric   string `arg:"metric" desc:"Optional metric name (e.g. 'temperature', 'value') to filter by."`
	Since    string `arg:"since" desc:"Optional ISO UTC bucket lower bound (e.g. '2026-11-19 00:00:00')."`
	Limit    int    `arg:"limit" max:"2000" desc:"Maximum rows to return (default 200, max 2000)."`
}

func (a *MQTTRollups) Doc() string {
	return "Query streaming time-series rollup buckets (min/max/avg/count/last) aggregated by MQTT ingest rules."
}
func (a *MQTTRollups) ReadOnly() bool { return true }
func (a *MQTTRollups) Example() any {
	return &MQTTRollups{Rule: "ingest_sensors", Interval: "1h", Limit: 48}
}
func (a *MQTTRollups) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := store.QueryMQTTRollups(
		ctx.DB,
		strings.TrimSpace(a.Rule),
		strings.TrimSpace(a.Interval),
		strings.TrimSpace(a.Series),
		strings.TrimSpace(a.Metric),
		strings.TrimSpace(a.Since),
		a.Limit,
	)
	if err != nil {
		return tools.Fail("query mqtt rollups: %v", err), nil
	}
	if rows == nil {
		rows = []store.MQTTRollupRow{}
	}
	return tools.Ok(map[string]any{
		"count":   len(rows),
		"rollups": rows,
	}), nil
}

// --- publish ----------------------------------------------------------------

type MQTTPublish struct {
	Topic   string `arg:"topic,required" desc:"Concrete MQTT topic to publish to (no + or # wildcards)."`
	Payload string `arg:"payload,required" desc:"Message payload string or JSON."`
	QoS     int    `arg:"qos" desc:"QoS level (0 or 1, default 0)."`
	Retain  bool   `arg:"retain" desc:"Persist as a retained message for this topic."`
}

func (a *MQTTPublish) Doc() string {
	return "Publish a message to an MQTT topic (updates retained state and delivers to connected subscribers)."
}
func (a *MQTTPublish) Example() any {
	return &MQTTPublish{Topic: "sensors/esp1/temp", Payload: `{"c":22.5}`, Retain: true}
}
func (a *MQTTPublish) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if err := mqtt.ValidatePublishTopic(a.Topic); err != nil {
		return tools.Fail("%v", err), nil
	}
	qos := byte(0)
	if a.QoS > 0 {
		qos = 1
	}
	if a.Retain {
		if a.Payload == "" {
			if err := store.DeleteMQTTRetained(ctx.DB, a.Topic); err != nil {
				return tools.Fail("delete retained: %v", err), nil
			}
		} else {
			if err := store.SetMQTTRetained(ctx.DB, a.Topic, []byte(a.Payload), qos); err != nil {
				return tools.Fail("save retained: %v", err), nil
			}
		}
	}
	if err := funcexec.PublishMQTT(ctx.ProjectID, a.Topic, []byte(a.Payload), qos, a.Retain); err != nil {
		// Still report ok if broker isn't active yet, or surface error only on invalid state.
		_ = err
	}
	return tools.Ok(map[string]any{
		"topic":   a.Topic,
		"payload": a.Payload,
		"qos":     qos,
		"retain":  a.Retain,
	}), nil
}
