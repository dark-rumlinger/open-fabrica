package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/dbh"
)

type mqttDB = dbh.Handle

// MQTTConfig stores per-project MQTT broker settings.
type MQTTConfig struct {
	Enabled        bool
	AllowAnonymous bool
	MaxClients     int
	UserAllowPub   string // CSV topic filters for JWT-authenticated users ("#" default, "none" = deny all; supports {user_id}, {tenant_id})
	UserAllowSub   string // CSV topic filters for JWT-authenticated users ("#" default, "none" = deny all; supports {user_id}, {tenant_id})
}

// MQTTDevice stores a registered MQTT device credential, topic ACL, and optional
// dead-man's-switch heartbeat watchdog interval.
type MQTTDevice struct {
	ID            int
	Name          string
	Username      string
	SecretHash    string
	AllowPub      string
	AllowSub      string
	HeartbeatSec  int
	StaleNotified bool
	Enabled       bool
	LastSeenAt    sql.NullTime
	CreatedAt     time.Time
}

// MQTTRetainedRow represents a persisted retained message.
type MQTTRetainedRow struct {
	Topic     string
	Payload   []byte
	QoS       byte
	UpdatedAt time.Time
}

// MQTTRule maps an incoming MQTT topic filter to a SQLite table insert,
// streaming time-series rollup, and/or sandboxed server function invocation.
type MQTTRule struct {
	ID             int
	Name           string
	TopicFilter    string
	TargetTable    string
	FunctionPath   string
	MinIntervalSec int
	RetentionDays  int
	RollupInterval string // "", "1m", "5m", "15m", "1h", "1d"
	Enabled        bool
	CreatedAt      time.Time
}

// MQTTRollupRow represents one aggregated time-series bucket in of_mqtt_rollups.
type MQTTRollupRow struct {
	RuleName    string    `json:"rule_name"`
	Bucket      string    `json:"bucket"`
	Interval    string    `json:"interval"`
	SeriesKey   string    `json:"series_key"`
	Metric      string    `json:"metric"`
	SampleCount int64     `json:"sample_count"`
	MinVal      float64   `json:"min"`
	MaxVal      float64   `json:"max"`
	SumVal      float64   `json:"sum"`
	AvgVal      float64   `json:"avg"`
	LastVal     float64   `json:"last"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MQTTBridge stores an outbound MQTT bridge connection to an external broker
// (e.g. LAN Mosquitto, Zigbee2MQTT, EMQX, Home Assistant, Frigate, Meshtastic).
type MQTTBridge struct {
	ID           int
	Name         string
	BrokerURL    string
	ClientID     string
	Username     string
	PasswordEnc  string
	RemoteSub    string
	LocalPrefix  string
	LocalPub     string
	RemotePrefix string
	QoS          byte
	Enabled      bool
	CreatedAt    time.Time
}

// LoadMQTTConfig loads the per-project MQTT configuration, returning defaults
// if no row exists yet.
func LoadMQTTConfig(reader mqttDB) (MQTTConfig, error) {
	cfg := MQTTConfig{MaxClients: 100, UserAllowPub: "#", UserAllowSub: "#"}
	var enabled, anon int
	err := reader.QueryRow(
		`SELECT enabled, allow_anonymous, max_clients, user_allow_pub, user_allow_sub FROM of_mqtt_config WHERE id = 1`,
	).Scan(&enabled, &anon, &cfg.MaxClients, &cfg.UserAllowPub, &cfg.UserAllowSub)
	if err == sql.ErrNoRows {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("load mqtt config: %w", err)
	}
	cfg.Enabled = enabled != 0
	cfg.AllowAnonymous = anon != 0
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = 100
	}
	if strings.TrimSpace(cfg.UserAllowPub) == "" {
		cfg.UserAllowPub = "#"
	}
	if strings.TrimSpace(cfg.UserAllowSub) == "" {
		cfg.UserAllowSub = "#"
	}
	return cfg, nil
}

// SaveMQTTConfig upserts the per-project MQTT configuration.
func SaveMQTTConfig(writer mqttDB, cfg MQTTConfig) error {
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = 100
	}
	if strings.TrimSpace(cfg.UserAllowPub) == "" {
		cfg.UserAllowPub = "#"
	}
	if strings.TrimSpace(cfg.UserAllowSub) == "" {
		cfg.UserAllowSub = "#"
	}
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_config (id, enabled, allow_anonymous, max_clients, user_allow_pub, user_allow_sub, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		   enabled = excluded.enabled,
		   allow_anonymous = excluded.allow_anonymous,
		   max_clients = excluded.max_clients,
		   user_allow_pub = excluded.user_allow_pub,
		   user_allow_sub = excluded.user_allow_sub,
		   updated_at = CURRENT_TIMESTAMP`,
		boolInt(cfg.Enabled), boolInt(cfg.AllowAnonymous), cfg.MaxClients, cfg.UserAllowPub, cfg.UserAllowSub,
	)
	return err
}

// UpsertMQTTDevice creates or updates an MQTT device credential, ACLs, and optional heartbeat watchdog.
func UpsertMQTTDevice(writer mqttDB, name, username, secretHash, allowPub, allowSub string, heartbeatSec int) error {
	if allowPub == "" {
		allowPub = "#"
	}
	if allowSub == "" {
		allowSub = "#"
	}
	if heartbeatSec < 0 {
		heartbeatSec = 0
	}
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_devices (name, username, secret_hash, allow_pub, allow_sub, heartbeat_sec, stale_notified, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 1)
		 ON CONFLICT(name) DO UPDATE SET
		   username = excluded.username,
		   secret_hash = excluded.secret_hash,
		   allow_pub = excluded.allow_pub,
		   allow_sub = excluded.allow_sub,
		   heartbeat_sec = excluded.heartbeat_sec,
		   stale_notified = 0,
		   enabled = 1`,
		name, username, secretHash, allowPub, allowSub, heartbeatSec,
	)
	return err
}

// LookupMQTTDevice fetches an enabled device by username.
func LookupMQTTDevice(reader mqttDB, username string) (MQTTDevice, bool, error) {
	var d MQTTDevice
	var enabled, stale int
	err := reader.QueryRow(
		`SELECT id, name, username, secret_hash, allow_pub, allow_sub, heartbeat_sec, stale_notified, enabled, last_seen_at, created_at
		 FROM of_mqtt_devices WHERE username = ? AND enabled = 1`,
		username,
	).Scan(&d.ID, &d.Name, &d.Username, &d.SecretHash, &d.AllowPub, &d.AllowSub, &d.HeartbeatSec, &stale, &enabled, &d.LastSeenAt, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d.StaleNotified = stale != 0
	d.Enabled = enabled != 0
	return d, true, nil
}

// TouchMQTTDevice updates last_seen_at for a device username and clears stale_notified.
// Returns (deviceName, true) if the device was previously marked stale and has now recovered.
func TouchMQTTDevice(writer mqttDB, username string) (string, bool) {
	if username == "" {
		return "", false
	}
	var name string
	var wasStale int
	err := writer.QueryRow(
		`SELECT name, stale_notified FROM of_mqtt_devices WHERE username = ?`,
		username,
	).Scan(&name, &wasStale)
	bestEffortExec(writer, "mqtt device last_seen",
		`UPDATE of_mqtt_devices SET last_seen_at = CURRENT_TIMESTAMP, stale_notified = 0 WHERE username = ?`,
		username,
	)
	return name, err == nil && wasStale != 0
}

// HasMQTTWatchdogs reports whether any enabled device has heartbeat_sec > 0.
func HasMQTTWatchdogs(reader mqttDB) bool {
	var n int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM of_mqtt_devices WHERE enabled = 1 AND heartbeat_sec > 0`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// FindAndMarkStaleMQTTDevices finds enabled devices whose last_seen_at has exceeded
// heartbeat_sec, marks stale_notified = 1 so alerts fire once, and returns them.
func FindAndMarkStaleMQTTDevices(writer mqttDB) ([]MQTTDevice, error) {
	rows, err := writer.Query(
		`SELECT id, name, username, secret_hash, allow_pub, allow_sub, heartbeat_sec, stale_notified, enabled, last_seen_at, created_at
		 FROM of_mqtt_devices
		 WHERE enabled = 1
		   AND heartbeat_sec > 0
		   AND stale_notified = 0
		   AND last_seen_at IS NOT NULL
		   AND last_seen_at < datetime('now', '-' || heartbeat_sec || ' seconds')`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []MQTTDevice
	for rows.Next() {
		var d MQTTDevice
		var st, en int
		if err := rows.Scan(&d.ID, &d.Name, &d.Username, &d.SecretHash, &d.AllowPub, &d.AllowSub, &d.HeartbeatSec, &st, &en, &d.LastSeenAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.StaleNotified = true
		d.Enabled = en != 0
		stale = append(stale, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range stale {
		bestEffortExec(writer, "mqtt mark stale", `UPDATE of_mqtt_devices SET stale_notified = 1 WHERE id = ?`, d.ID)
	}
	return stale, nil
}

// ListMQTTDevices lists all registered MQTT devices.
func ListMQTTDevices(reader mqttDB) ([]MQTTDevice, error) {
	rows, err := reader.Query(
		`SELECT id, name, username, secret_hash, allow_pub, allow_sub, heartbeat_sec, stale_notified, enabled, last_seen_at, created_at
		 FROM of_mqtt_devices ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MQTTDevice
	for rows.Next() {
		var d MQTTDevice
		var stale, enabled int
		if err := rows.Scan(&d.ID, &d.Name, &d.Username, &d.SecretHash, &d.AllowPub, &d.AllowSub, &d.HeartbeatSec, &stale, &enabled, &d.LastSeenAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.StaleNotified = stale != 0
		d.Enabled = enabled != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteMQTTDevice removes a device by name.
func DeleteMQTTDevice(writer mqttDB, name string) (bool, error) {
	res, err := writer.Exec(`DELETE FROM of_mqtt_devices WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetMQTTRetained upserts a retained MQTT message.
func SetMQTTRetained(writer mqttDB, topic string, payload []byte, qos byte) error {
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_retained (topic, payload, qos, updated_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(topic) DO UPDATE SET
		   payload = excluded.payload,
		   qos = excluded.qos,
		   updated_at = CURRENT_TIMESTAMP`,
		topic, payload, int(qos),
	)
	return err
}

// DeleteMQTTRetained deletes a retained message by topic.
func DeleteMQTTRetained(writer mqttDB, topic string) error {
	_, err := writer.Exec(`DELETE FROM of_mqtt_retained WHERE topic = ?`, topic)
	return err
}

// ListMQTTRetained returns all retained messages ordered by topic.
func ListMQTTRetained(reader mqttDB) ([]MQTTRetainedRow, error) {
	rows, err := reader.Query(`SELECT topic, payload, qos, updated_at FROM of_mqtt_retained ORDER BY topic`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MQTTRetainedRow
	for rows.Next() {
		var r MQTTRetainedRow
		var qos int
		if err := rows.Scan(&r.Topic, &r.Payload, &qos, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.QoS = byte(qos)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertMQTTRule creates or updates an MQTT ingest rule.
func UpsertMQTTRule(writer mqttDB, r MQTTRule) error {
	if r.MinIntervalSec < 0 {
		r.MinIntervalSec = 0
	}
	if r.RetentionDays < 0 {
		r.RetentionDays = 0
	}
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_rules
		 (name, topic_filter, target_table, function_path, min_interval_sec, retention_days, rollup_interval, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT(name) DO UPDATE SET
		   topic_filter = excluded.topic_filter,
		   target_table = excluded.target_table,
		   function_path = excluded.function_path,
		   min_interval_sec = excluded.min_interval_sec,
		   retention_days = excluded.retention_days,
		   rollup_interval = excluded.rollup_interval,
		   enabled = 1`,
		r.Name, r.TopicFilter, r.TargetTable, r.FunctionPath, r.MinIntervalSec, r.RetentionDays, r.RollupInterval,
	)
	return err
}

// ListMQTTRules lists all MQTT ingest rules.
func ListMQTTRules(reader mqttDB) ([]MQTTRule, error) {
	rows, err := reader.Query(
		`SELECT id, name, topic_filter, target_table, function_path,
		        min_interval_sec, retention_days, rollup_interval, enabled, created_at
		 FROM of_mqtt_rules ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MQTTRule
	for rows.Next() {
		var r MQTTRule
		var enabled int
		if err := rows.Scan(
			&r.ID, &r.Name, &r.TopicFilter, &r.TargetTable, &r.FunctionPath,
			&r.MinIntervalSec, &r.RetentionDays, &r.RollupInterval, &enabled, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertMQTTRollup incrementally merges one numeric sample into its time bucket.
func UpsertMQTTRollup(writer mqttDB, ruleName, interval, bucket, seriesKey, metric string, val float64) {
	bestEffortExec(writer, "mqtt rollup upsert",
		`INSERT INTO of_mqtt_rollups
		 (rule_name, bucket, interval, series_key, metric, sample_count, min_val, max_val, sum_val, avg_val, last_val, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(rule_name, interval, bucket, series_key, metric) DO UPDATE SET
		   sample_count = of_mqtt_rollups.sample_count + 1,
		   min_val = MIN(of_mqtt_rollups.min_val, excluded.min_val),
		   max_val = MAX(of_mqtt_rollups.max_val, excluded.max_val),
		   sum_val = of_mqtt_rollups.sum_val + excluded.sum_val,
		   avg_val = (of_mqtt_rollups.sum_val + excluded.sum_val) / (of_mqtt_rollups.sample_count + 1),
		   last_val = excluded.last_val,
		   updated_at = CURRENT_TIMESTAMP`,
		ruleName, bucket, interval, seriesKey, metric, val, val, val, val, val,
	)
}

// QueryMQTTRollups queries aggregated time-series buckets from of_mqtt_rollups.
func QueryMQTTRollups(reader mqttDB, ruleName, interval, seriesKey, metric, since string, limit int) ([]MQTTRollupRow, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	var where []string
	var args []any
	if ruleName != "" {
		where = append(where, "rule_name = ?")
		args = append(args, ruleName)
	}
	if interval != "" {
		where = append(where, "interval = ?")
		args = append(args, interval)
	}
	if seriesKey != "" {
		where = append(where, "series_key = ?")
		args = append(args, seriesKey)
	}
	if metric != "" {
		where = append(where, "metric = ?")
		args = append(args, metric)
	}
	if since != "" {
		where = append(where, "bucket >= ?")
		args = append(args, since)
	}
	q := `SELECT rule_name, bucket, interval, series_key, metric, sample_count, min_val, max_val, sum_val, avg_val, last_val, updated_at
	      FROM of_mqtt_rollups`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY bucket DESC LIMIT ?"
	args = append(args, limit)

	rows, err := reader.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MQTTRollupRow
	for rows.Next() {
		var r MQTTRollupRow
		if err := rows.Scan(
			&r.RuleName, &r.Bucket, &r.Interval, &r.SeriesKey, &r.Metric,
			&r.SampleCount, &r.MinVal, &r.MaxVal, &r.SumVal, &r.AvgVal, &r.LastVal, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MQTTOfflineRow represents a queued QoS 1 message waiting for a sleeping client.
type MQTTOfflineRow struct {
	ID      int64
	Topic   string
	Payload []byte
	QoS     byte
}

// SaveMQTTSubscription persists a cleanSession=false client subscription.
func SaveMQTTSubscription(writer mqttDB, clientID, filter string, qos byte) error {
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_subscriptions (client_id, topic_filter, qos)
		 VALUES (?, ?, ?)
		 ON CONFLICT(client_id, topic_filter) DO UPDATE SET qos = excluded.qos`,
		clientID, filter, int(qos),
	)
	return err
}

// DeleteMQTTSubscription removes one persisted subscription for clientID.
func DeleteMQTTSubscription(writer mqttDB, clientID, filter string) error {
	_, err := writer.Exec(`DELETE FROM of_mqtt_subscriptions WHERE client_id = ? AND topic_filter = ?`, clientID, filter)
	return err
}

// ClearMQTTSession deletes persisted subscriptions and queued offline messages for clientID.
func ClearMQTTSession(writer mqttDB, clientID string) error {
	if _, err := writer.Exec(`DELETE FROM of_mqtt_subscriptions WHERE client_id = ?`, clientID); err != nil {
		return err
	}
	_, err := writer.Exec(`DELETE FROM of_mqtt_offline_queue WHERE client_id = ?`, clientID)
	return err
}

// ListMQTTSubscriptions loads all persisted cleanSession=false subscriptions.
func ListMQTTSubscriptions(reader mqttDB) (map[string]map[string]byte, error) {
	rows, err := reader.Query(`SELECT client_id, topic_filter, qos FROM of_mqtt_subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]byte)
	for rows.Next() {
		var cid, filter string
		var qos int
		if err := rows.Scan(&cid, &filter, &qos); err != nil {
			return nil, err
		}
		if out[cid] == nil {
			out[cid] = make(map[string]byte)
		}
		out[cid][filter] = byte(qos)
	}
	return out, rows.Err()
}

// EnqueueMQTTOffline appends a QoS 1 message for an offline cleanSession=false client,
// keeping at most maxPerClient newest messages per client.
func EnqueueMQTTOffline(writer mqttDB, clientID, topic string, payload []byte, qos byte, maxPerClient int) error {
	if maxPerClient <= 0 {
		maxPerClient = 100
	}
	if _, err := writer.Exec(
		`INSERT INTO of_mqtt_offline_queue (client_id, topic, payload, qos) VALUES (?, ?, ?, ?)`,
		clientID, topic, payload, int(qos),
	); err != nil {
		return err
	}
	// Evict oldest overflow messages beyond maxPerClient so a permanently offline device cannot bloat SQLite.
	bestEffortExec(writer, "mqtt offline queue trim",
		`DELETE FROM of_mqtt_offline_queue
		 WHERE client_id = ?
		   AND id NOT IN (
		       SELECT id FROM of_mqtt_offline_queue WHERE client_id = ? ORDER BY id DESC LIMIT ?
		   )`,
		clientID, clientID, maxPerClient,
	)
	return nil
}

// DrainMQTTOffline returns up to limit queued offline messages for clientID in FIFO order
// and deletes them from the queue.
func DrainMQTTOffline(writer mqttDB, clientID string, limit int) ([]MQTTOfflineRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := writer.Query(
		`SELECT id, topic, payload, qos FROM of_mqtt_offline_queue WHERE client_id = ? ORDER BY id ASC LIMIT ?`,
		clientID, limit,
	)
	if err != nil {
		return nil, err
	}
	var out []MQTTOfflineRow
	for rows.Next() {
		var r MQTTOfflineRow
		var qos int
		if err := rows.Scan(&r.ID, &r.Topic, &r.Payload, &qos); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.QoS = byte(qos)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(out) > 0 {
		lastID := out[len(out)-1].ID
		bestEffortExec(writer, "mqtt offline queue drain",
			`DELETE FROM of_mqtt_offline_queue WHERE client_id = ? AND id <= ?`,
			clientID, lastID,
		)
	}
	return out, nil
}

// PruneMQTTTableOlderThan deletes rows older than retentionDays from a validated
// user table using its validated timestamp column.
func PruneMQTTTableOlderThan(writer mqttDB, table, tsCol string, retentionDays int) {
	if retentionDays <= 0 || table == "" || tsCol == "" {
		return
	}
	// #nosec G201 G701 -- table and column identifiers are validated by caller against PRAGMA table_info
	q := fmt.Sprintf(`DELETE FROM "%s" WHERE "%s" < datetime('now', '-' || ? || ' days')`, table, tsCol)
	bestEffortExec(writer, "mqtt retention prune", q, retentionDays)
}

// DeleteMQTTRule removes an ingest rule by name.
func DeleteMQTTRule(writer mqttDB, name string) (bool, error) {
	res, err := writer.Exec(`DELETE FROM of_mqtt_rules WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpsertMQTTBridge creates or updates an outbound MQTT bridge definition.
func UpsertMQTTBridge(writer mqttDB, b MQTTBridge) error {
	qos := int(b.QoS)
	if qos > 1 {
		qos = 1
	}
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_bridges
		 (name, broker_url, client_id, username, password_enc, remote_sub, local_prefix, local_pub, remote_prefix, qos, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT(name) DO UPDATE SET
		   broker_url = excluded.broker_url,
		   client_id = excluded.client_id,
		   username = excluded.username,
		   password_enc = excluded.password_enc,
		   remote_sub = excluded.remote_sub,
		   local_prefix = excluded.local_prefix,
		   local_pub = excluded.local_pub,
		   remote_prefix = excluded.remote_prefix,
		   qos = excluded.qos,
		   enabled = 1`,
		b.Name, b.BrokerURL, b.ClientID, b.Username, b.PasswordEnc,
		b.RemoteSub, b.LocalPrefix, b.LocalPub, b.RemotePrefix, qos,
	)
	return err
}

// ListMQTTBridges lists all configured outbound MQTT bridges.
func ListMQTTBridges(reader mqttDB) ([]MQTTBridge, error) {
	rows, err := reader.Query(
		`SELECT id, name, broker_url, client_id, username, password_enc,
		        remote_sub, local_prefix, local_pub, remote_prefix, qos, enabled, created_at
		 FROM of_mqtt_bridges ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MQTTBridge
	for rows.Next() {
		var b MQTTBridge
		var qos, enabled int
		if err := rows.Scan(
			&b.ID, &b.Name, &b.BrokerURL, &b.ClientID, &b.Username, &b.PasswordEnc,
			&b.RemoteSub, &b.LocalPrefix, &b.LocalPub, &b.RemotePrefix, &qos, &enabled, &b.CreatedAt,
		); err != nil {
			return nil, err
		}
		b.QoS = byte(qos)
		b.Enabled = enabled != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteMQTTBridge removes an outbound bridge by name.
func DeleteMQTTBridge(writer mqttDB, name string) (bool, error) {
	res, err := writer.Exec(`DELETE FROM of_mqtt_bridges WHERE name = ?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// LookupProjectIDBySlug resolves a project ID from its slug in globalDB.
func LookupProjectIDBySlug(globalDB mqttDB, slug string) (int, error) {
	var pid int
	err := globalDB.QueryRow(`SELECT id FROM projects WHERE slug = ?`, slug).Scan(&pid)
	return pid, err
}

// ListProjectIDs returns all project IDs in globalDB ordered by id.
func ListProjectIDs(globalDB mqttDB) ([]int, error) {
	rows, err := globalDB.Query(`SELECT id FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// InsertMQTTRow inserts a validated table/column map from an MQTT publish rule.
func InsertMQTTRow(writer mqttDB, table string, cols, placeholders []string, args []any) {
	if len(cols) == 0 {
		return
	}
	// #nosec G201 G701 -- table and column identifiers are validated by caller; values are parameterized
	q := `INSERT INTO "` + table + `" (` + strings.Join(cols, ", ") + `) VALUES (` + strings.Join(placeholders, ", ") + `)`
	bestEffortExec(writer, "mqtt rule insert", q, args...)
}
