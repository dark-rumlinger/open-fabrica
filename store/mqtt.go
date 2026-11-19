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
}

// MQTTDevice stores a registered MQTT device credential and topic ACL.
type MQTTDevice struct {
	ID         int
	Name       string
	Username   string
	SecretHash string
	AllowPub   string
	AllowSub   string
	Enabled    bool
	LastSeenAt sql.NullTime
	CreatedAt  time.Time
}

// MQTTRetainedRow represents a persisted retained message.
type MQTTRetainedRow struct {
	Topic     string
	Payload   []byte
	QoS       byte
	UpdatedAt time.Time
}

// MQTTRule maps an incoming MQTT topic filter to a SQLite table insert and/or
// a sandboxed server function invocation.
type MQTTRule struct {
	ID           int
	Name         string
	TopicFilter  string
	TargetTable  string
	FunctionPath string
	Enabled      bool
	CreatedAt    time.Time
}

// LoadMQTTConfig loads the per-project MQTT configuration, returning defaults
// if no row exists yet.
func LoadMQTTConfig(reader mqttDB) (MQTTConfig, error) {
	cfg := MQTTConfig{MaxClients: 100}
	var enabled, anon int
	err := reader.QueryRow(
		`SELECT enabled, allow_anonymous, max_clients FROM of_mqtt_config WHERE id = 1`,
	).Scan(&enabled, &anon, &cfg.MaxClients)
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
	return cfg, nil
}

// SaveMQTTConfig upserts the per-project MQTT configuration.
func SaveMQTTConfig(writer mqttDB, cfg MQTTConfig) error {
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = 100
	}
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_config (id, enabled, allow_anonymous, max_clients, updated_at)
		 VALUES (1, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		   enabled = excluded.enabled,
		   allow_anonymous = excluded.allow_anonymous,
		   max_clients = excluded.max_clients,
		   updated_at = CURRENT_TIMESTAMP`,
		boolInt(cfg.Enabled), boolInt(cfg.AllowAnonymous), cfg.MaxClients,
	)
	return err
}

// UpsertMQTTDevice creates or updates an MQTT device credential and ACLs.
func UpsertMQTTDevice(writer mqttDB, name, username, secretHash, allowPub, allowSub string) error {
	if allowPub == "" {
		allowPub = "#"
	}
	if allowSub == "" {
		allowSub = "#"
	}
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_devices (name, username, secret_hash, allow_pub, allow_sub, enabled)
		 VALUES (?, ?, ?, ?, ?, 1)
		 ON CONFLICT(name) DO UPDATE SET
		   username = excluded.username,
		   secret_hash = excluded.secret_hash,
		   allow_pub = excluded.allow_pub,
		   allow_sub = excluded.allow_sub,
		   enabled = 1`,
		name, username, secretHash, allowPub, allowSub,
	)
	return err
}

// LookupMQTTDevice fetches an enabled device by username.
func LookupMQTTDevice(reader mqttDB, username string) (MQTTDevice, bool, error) {
	var d MQTTDevice
	var enabled int
	err := reader.QueryRow(
		`SELECT id, name, username, secret_hash, allow_pub, allow_sub, enabled, last_seen_at, created_at
		 FROM of_mqtt_devices WHERE username = ? AND enabled = 1`,
		username,
	).Scan(&d.ID, &d.Name, &d.Username, &d.SecretHash, &d.AllowPub, &d.AllowSub, &enabled, &d.LastSeenAt, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d.Enabled = enabled != 0
	return d, true, nil
}

// TouchMQTTDevice updates last_seen_at for a device username.
func TouchMQTTDevice(writer mqttDB, username string) {
	bestEffortExec(writer, "mqtt device last_seen", `UPDATE of_mqtt_devices SET last_seen_at = CURRENT_TIMESTAMP WHERE username = ?`, username)
}

// ListMQTTDevices lists all registered MQTT devices.
func ListMQTTDevices(reader mqttDB) ([]MQTTDevice, error) {
	rows, err := reader.Query(
		`SELECT id, name, username, secret_hash, allow_pub, allow_sub, enabled, last_seen_at, created_at
		 FROM of_mqtt_devices ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MQTTDevice
	for rows.Next() {
		var d MQTTDevice
		var enabled int
		if err := rows.Scan(&d.ID, &d.Name, &d.Username, &d.SecretHash, &d.AllowPub, &d.AllowSub, &enabled, &d.LastSeenAt, &d.CreatedAt); err != nil {
			return nil, err
		}
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
func UpsertMQTTRule(writer mqttDB, name, topicFilter, targetTable, functionPath string) error {
	_, err := writer.Exec(
		`INSERT INTO of_mqtt_rules (name, topic_filter, target_table, function_path, enabled)
		 VALUES (?, ?, ?, ?, 1)
		 ON CONFLICT(name) DO UPDATE SET
		   topic_filter = excluded.topic_filter,
		   target_table = excluded.target_table,
		   function_path = excluded.function_path,
		   enabled = 1`,
		name, topicFilter, targetTable, functionPath,
	)
	return err
}

// ListMQTTRules lists all MQTT ingest rules.
func ListMQTTRules(reader mqttDB) ([]MQTTRule, error) {
	rows, err := reader.Query(
		`SELECT id, name, topic_filter, target_table, function_path, enabled, created_at
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
		if err := rows.Scan(&r.ID, &r.Name, &r.TopicFilter, &r.TargetTable, &r.FunctionPath, &enabled, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
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
