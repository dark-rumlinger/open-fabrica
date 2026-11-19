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
		OneLiner:           "Configure the project's built-in MQTT broker (devices, topic ACLs, SQLite/function ingest rules, and test publish).",
		WhenToUse: `Use when building IoT hubs, sensor telemetry dashboards, Raspberry Pi/ESP32
controllers, or device pub/sub backends. Supports MQTT over WebSocket at /mqtt
(and raw TCP when OPENFABRICA_MQTT_PORT is set). Actions: configure, status,
add_device, delete_device, add_rule, delete_rule, publish.`,
		Actions: []tools.Action{
			&MQTTConfigure{},
			&MQTTStatus{},
			&MQTTAddDevice{},
			&MQTTDeleteDevice{},
			&MQTTAddRule{},
			&MQTTDeleteRule{},
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
	Enabled        bool `arg:"enabled,required" desc:"Enable or disable the project's MQTT broker (/mqtt WebSocket and TCP routing)."`
	AllowAnonymous bool `arg:"allow_anonymous" desc:"Allow unauthenticated connections (for local testing or public read-only demos). Default false."`
	MaxClients     int  `arg:"max_clients" desc:"Maximum concurrent MQTT clients for this project (default 100)."`
}

func (a *MQTTConfigure) Doc() string {
	return "Enable or configure the project's MQTT broker."
}
func (a *MQTTConfigure) Example() any {
	return &MQTTConfigure{Enabled: true, AllowAnonymous: false, MaxClients: 100}
}
func (a *MQTTConfigure) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	maxC := a.MaxClients
	if maxC <= 0 {
		maxC = 100
	}
	cfg := store.MQTTConfig{
		Enabled:        a.Enabled,
		AllowAnonymous: a.AllowAnonymous,
		MaxClients:     maxC,
	}
	if err := store.SaveMQTTConfig(ctx.DB, cfg); err != nil {
		return tools.Fail("save mqtt config: %v", err), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"enabled":         cfg.Enabled,
		"allow_anonymous": cfg.AllowAnonymous,
		"max_clients":     cfg.MaxClients,
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

	devViews := make([]map[string]any, len(devices))
	for i, d := range devices {
		devViews[i] = map[string]any{
			"name":      d.Name,
			"username":  d.Username,
			"allow_pub": d.AllowPub,
			"allow_sub": d.AllowSub,
			"enabled":   d.Enabled,
		}
	}
	ruleViews := make([]map[string]any, len(rules))
	for i, r := range rules {
		ruleViews[i] = map[string]any{
			"name":          r.Name,
			"topic_filter":  r.TopicFilter,
			"target_table":  r.TargetTable,
			"function_path": r.FunctionPath,
			"enabled":       r.Enabled,
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

	return tools.Ok(map[string]any{
		"enabled":         cfg.Enabled,
		"allow_anonymous": cfg.AllowAnonymous,
		"max_clients":     cfg.MaxClients,
		"ws_path":         "/mqtt",
		"devices":         devViews,
		"rules":           ruleViews,
		"retained":        retViews,
	}), nil
}

// --- add_device -------------------------------------------------------------

type MQTTAddDevice struct {
	Name     string `arg:"name,required" desc:"Human-readable device identifier (e.g. greenhouse_esp32)."`
	Username string `arg:"username,required" desc:"MQTT CONNECT username for this device."`
	Token    string `arg:"token" sensitive:"true" desc:"Optional password/token. Leave blank to auto-generate a secure token (returned once)."`
	AllowPub string `arg:"allow_pub" desc:"Comma-separated MQTT topic filters this device may publish to (default '#')."`
	AllowSub string `arg:"allow_sub" desc:"Comma-separated MQTT topic filters this device may subscribe to (default '#')."`
}

func (a *MQTTAddDevice) Doc() string {
	return "Register or update an MQTT device credential with topic ACLs."
}
func (a *MQTTAddDevice) Example() any {
	return &MQTTAddDevice{
		Name:     "greenhouse_esp32",
		Username: "esp32_1",
		AllowPub: "sensors/esp32_1/#",
		AllowSub: "actuators/esp32_1/#",
	}
}
func (a *MQTTAddDevice) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.TrimSpace(a.Name)
	user := strings.TrimSpace(a.Username)
	if name == "" || user == "" {
		return tools.Fail("name and username are required"), nil
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

	if err := store.UpsertMQTTDevice(ctx.DB, name, user, hashDeviceToken(token), allowPub, allowSub); err != nil {
		return tools.Fail("save mqtt device: %v", err), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"name":      name,
		"username":  user,
		"token":     token,
		"allow_pub": allowPub,
		"allow_sub": allowSub,
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
	Name         string `arg:"name,required" desc:"Unique rule name (e.g. ingest_sensors)."`
	TopicFilter  string `arg:"topic_filter,required" desc:"MQTT topic filter to match (e.g. sensors/+/reading)."`
	TargetTable  string `arg:"target_table" desc:"Optional SQLite table name to auto-insert JSON payloads into."`
	FunctionPath string `arg:"function_path" desc:"Optional manage_function endpoint path to invoke with {topic, payload, json, device}."`
}

func (a *MQTTAddRule) Doc() string {
	return "Add or update a topic ingest rule mapping MQTT messages to a SQLite table and/or server function."
}
func (a *MQTTAddRule) Example() any {
	return &MQTTAddRule{
		Name:         "ingest_sensors",
		TopicFilter:  "sensors/+/reading",
		TargetTable:  "readings",
		FunctionPath: "on-sensor-reading",
	}
}
func (a *MQTTAddRule) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	name := strings.TrimSpace(a.Name)
	filter := strings.TrimSpace(a.TopicFilter)
	table := strings.TrimSpace(a.TargetTable)
	fnPath := strings.TrimSpace(a.FunctionPath)
	if name == "" || filter == "" {
		return tools.Fail("name and topic_filter are required"), nil
	}
	if err := mqtt.ValidateTopicFilter(filter); err != nil {
		return tools.Fail("invalid topic_filter %q: %v", filter, err), nil
	}
	if table == "" && fnPath == "" {
		return tools.Fail("at least one of target_table or function_path is required"), nil
	}
	if strings.HasPrefix(strings.ToLower(table), "of_") {
		return tools.Fail("target_table cannot be a reserved of_ table"), nil
	}
	if err := store.UpsertMQTTRule(ctx.DB, name, filter, table, fnPath); err != nil {
		return tools.Fail("save mqtt rule: %v", err), nil
	}
	emitMQTTConfigChanged(ctx)
	return tools.Ok(map[string]any{
		"name":          name,
		"topic_filter":  filter,
		"target_table":  table,
		"function_path": fnPath,
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
