package public

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"openfabrica/internal/mqtt"
	"openfabrica/security"
	"openfabrica/store"
)

func openTestMQTTDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	stmts := []string{
		`CREATE TABLE of_dynamic_tables (table_name TEXT PRIMARY KEY, secure_columns TEXT)`,
		`CREATE TABLE readings (id INTEGER PRIMARY KEY AUTOINCREMENT, topic TEXT, device TEXT, zone TEXT, c REAL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE strict_readings (id INTEGER PRIMARY KEY AUTOINCREMENT, metric TEXT NOT NULL, value REAL NOT NULL, recorded_at TEXT NOT NULL)`,
		`CREATE TABLE scalar_readings (id INTEGER PRIMARY KEY AUTOINCREMENT, topic TEXT, device TEXT, payload TEXT, value REAL)`,
		`CREATE TABLE of_mqtt_rollups (
			rule_name TEXT NOT NULL, bucket TEXT NOT NULL, interval TEXT NOT NULL,
			series_key TEXT NOT NULL, metric TEXT NOT NULL, sample_count INTEGER NOT NULL DEFAULT 0,
			min_val REAL NOT NULL, max_val REAL NOT NULL, sum_val REAL NOT NULL,
			avg_val REAL NOT NULL, last_val REAL NOT NULL, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (rule_name, interval, bucket, series_key, metric)
		)`,
		`CREATE TABLE of_mqtt_devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, username TEXT NOT NULL UNIQUE,
			secret_hash TEXT NOT NULL, allow_pub TEXT NOT NULL DEFAULT '#', allow_sub TEXT NOT NULL DEFAULT '#',
			heartbeat_sec INTEGER NOT NULL DEFAULT 0, stale_notified INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1, last_seen_at DATETIME, created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestInsertMQTTTableRow_JSONAndScalarMetadata(t *testing.T) {
	d := openTestMQTTDB(t)

	// 1. JSON payload with extra unknown key + captured topic segment {zone}.
	payloadJSON := []byte(`{"c": 22.5, "unknown_extra": "skip_me"}`)
	var parsed any
	_ = json.Unmarshal(payloadJSON, &parsed)
	ev := mqtt.PublishEvent{
		ClientID:   "esp-1",
		DeviceName: "greenhouse_esp32",
		Topic:      "sensors/zone1/temp",
		Payload:    payloadJSON,
	}
	insertMQTTTableRow(d, d, nil, "readings", ev, parsed.(map[string]any), parsed, map[string]string{"zone": "zone1"})

	var topic, device, zone string
	var c float64
	if err := d.QueryRow(`SELECT topic, device, zone, c FROM readings`).Scan(&topic, &device, &zone, &c); err != nil {
		t.Fatalf("expected row in readings: %v", err)
	}
	if topic != "sensors/zone1/temp" || device != "greenhouse_esp32" || zone != "zone1" || c != 22.5 {
		t.Fatalf("unexpected readings row: topic=%q device=%q zone=%q c=%v", topic, device, zone, c)
	}

	// 2. Raw numeric scalar payload -> auto-fills topic, device, payload, and numeric value.
	scalarPayload := []byte("23.75")
	var parsedScalar any
	_ = json.Unmarshal(scalarPayload, &parsedScalar)
	ev2 := mqtt.PublishEvent{
		ClientID:   "esp-2",
		DeviceName: "freezer_esp32",
		Topic:      "sensors/freezer/temp",
		Payload:    scalarPayload,
	}
	insertMQTTTableRow(d, d, nil, "scalar_readings", ev2, nil, parsedScalar, nil)

	var rawPayload string
	var val float64
	if err := d.QueryRow(`SELECT topic, device, payload, value FROM scalar_readings`).Scan(&topic, &device, &rawPayload, &val); err != nil {
		t.Fatalf("expected row in scalar_readings: %v", err)
	}
	if topic != "sensors/freezer/temp" || device != "freezer_esp32" || rawPayload != "23.75" || val != 23.75 {
		t.Fatalf("unexpected scalar row: topic=%q device=%q payload=%q value=%v", topic, device, rawPayload, val)
	}

	// 3. NOT NULL timestamp column (recorded_at TEXT NOT NULL) omitted by payload -> auto-filled with ISO timestamp.
	strictPayload := []byte(`{"value": 19.5}`)
	var parsedStrict any
	_ = json.Unmarshal(strictPayload, &parsedStrict)
	ev3 := mqtt.PublishEvent{
		ClientID: "server",
		Topic:    "greenhouse/sensors/temperature",
		Payload:  strictPayload,
	}
	insertMQTTTableRow(d, d, nil, "strict_readings", ev3, parsedStrict.(map[string]any), parsedStrict, map[string]string{"metric": "temperature"})

	var metric, recAt string
	if err := d.QueryRow(`SELECT metric, value, recorded_at FROM strict_readings`).Scan(&metric, &val, &recAt); err != nil {
		t.Fatalf("expected row in strict_readings: %v", err)
	}
	if metric != "temperature" || val != 19.5 || recAt == "" {
		t.Fatalf("unexpected strict_readings row: metric=%q val=%v recorded_at=%q", metric, val, recAt)
	}
}

func TestRecordMQTTRollup_AndPrune(t *testing.T) {
	d := openTestMQTTDB(t)
	rule := store.MQTTRule{Name: "ingest_sensors", RollupInterval: "1h"}
	now := time.Date(2026, 11, 19, 20, 15, 0, 0, time.UTC)

	ev := mqtt.PublishEvent{Topic: "zigbee2mqtt/living_room"}
	recordMQTTRollup(d, rule, ev, map[string]any{"temperature": 20.0, "humidity": 50.0}, nil, now)
	recordMQTTRollup(d, rule, ev, map[string]any{"temperature": 24.0, "humidity": 60.0}, nil, now.Add(10*time.Minute))

	rows, err := store.QueryMQTTRollups(d, "ingest_sensors", "1h", "zigbee2mqtt/living_room", "temperature", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 rollup bucket, got %d", len(rows))
	}
	r := rows[0]
	if r.SampleCount != 2 || r.MinVal != 20.0 || r.MaxVal != 24.0 || r.AvgVal != 22.0 || r.LastVal != 24.0 {
		t.Fatalf("unexpected rollup stats: %+v", r)
	}

	// Retention pruning removes old rows and keeps recent ones.
	_, _ = d.Exec(`INSERT INTO readings (topic, device, zone, c, created_at) VALUES ('t', 'd', 'z', 10, datetime('now', '-10 days'))`)
	_, _ = d.Exec(`INSERT INTO readings (topic, device, zone, c, created_at) VALUES ('t', 'd', 'z', 20, datetime('now'))`)
	pruneMQTTTable(d, d, "readings", 7)
	var remaining int
	if err := d.QueryRow(`SELECT COUNT(*) FROM readings`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("expected 1 recent row remaining after 7-day prune, got %d", remaining)
	}
}

func TestExpandUserMQTTACL(t *testing.T) {
	admin := &security.Claims{UserID: 1, Role: "admin"}
	if got := expandUserMQTTACL("none", admin); got != "#" {
		t.Fatalf("admin should get #, got %q", got)
	}

	user := &security.Claims{UserID: 42, TenantID: 7, Role: "user"}
	if got := expandUserMQTTACL("commands/user/{user_id}/#,tenant/{tenant_id}/+", user); got != "commands/user/42/#,tenant/7/+" {
		t.Fatalf("unexpected expanded ACL: %q", got)
	}
	if got := expandUserMQTTACL("none", user); got != "" {
		t.Fatalf("expected empty ACL for 'none', got %q", got)
	}

	noTenant := &security.Claims{UserID: 42, TenantID: 0, Role: "user"}
	if got := expandUserMQTTACL("tenant/{tenant_id}/#,public/#", noTenant); got != "public/#" {
		t.Fatalf("expected tenant-scoped filter dropped when TenantID=0, got %q", got)
	}
}

func TestWatchdog_StaleAndRecovered(t *testing.T) {
	d := openTestMQTTDB(t)
	if err := store.UpsertMQTTDevice(d, "freezer_1", "freezer1", "hash", "#", "#", 60); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE of_mqtt_devices SET last_seen_at = datetime('now', '-120 seconds') WHERE username = 'freezer1'`); err != nil {
		t.Fatal(err)
	}

	stale, err := store.FindAndMarkStaleMQTTDevices(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Name != "freezer_1" {
		t.Fatalf("expected freezer_1 stale, got %+v", stale)
	}

	// Second check must not re-fire while already marked stale.
	stale2, err := store.FindAndMarkStaleMQTTDevices(d)
	if err != nil || len(stale2) != 0 {
		t.Fatalf("expected 0 duplicate stale alerts, got %+v err=%v", stale2, err)
	}

	// Device reports again -> TouchMQTTDevice clears stale_notified and reports recovered=true.
	name, recovered := store.TouchMQTTDevice(d, "freezer1")
	if !recovered || name != "freezer_1" {
		t.Fatalf("expected recovered=true for freezer_1, got name=%q recovered=%v", name, recovered)
	}
}
