---
slug: mqtt
role: capability
title: MQTT broker and IoT device pub/sub
summary: Built-in per-project MQTT 3.1.1 broker (/mqtt WebSocket and optional TCP), device ACLs, SQLite/function ingest rules, and ctx.mqtt.publish.
use_when: The app is an IoT hub, sensor telemetry dashboard, Raspberry Pi/ESP32 controller, or device pub/sub backend.
not_when: Browser-to-browser chat, rooms, or multiplayer games - use manage_relay or manage_authoritative.
evidence_gated: true
category: realtime
position: 35
tools: [manage_mqtt]
---

# MQTT Broker (`manage_mqtt`)

Open Fabrica includes a built-in, per-project **MQTT 3.1.1 broker** (QoS 0 & 1, `+`/`#` wildcards, persisted retained messages, Last Will & Testament, and device ACLs).

## Transports

- **WebSocket (`/mqtt`):** always available on the project's public HTTP/HTTPS port (`wss://<domain>/mqtt` or `ws://<host>/mqtt`), using subprotocol `mqtt` or `mqttv3.1`. Browser dashboards can connect using a signed-in user's JWT (`password = localStorage.getItem('auth_token')`) or a device credential.
- **Raw TCP (`OPENFABRICA_MQTT_PORT`, e.g. `1883`):** for ESP32 / Tasmota / embedded clients. Devices connect with `username = "<project_slug>:<device_username>"` (or `<device_username>` if unique) and `password = "<device_token>"`.

## Setup Flow

1. **Enable the broker:** `manage_mqtt configure { enabled: true, allow_anonymous: false, user_allow_pub: "commands/user/{user_id}/#", user_allow_sub: "sensors/#" }` (`user_allow_pub`/`user_allow_sub` scope non-admin JWT users; use `"none"` for read-only dashboards; defaults to `"#"`).
2. **Register devices:** `manage_mqtt add_device { name: "esp1", username: "esp1", allow_pub: "sensors/esp1/#", allow_sub: "actuators/esp1/#", heartbeat_sec: 300 }`. Save or show the generated `token` to the owner once.
   - Setting `heartbeat_sec` enables a dead-man's-switch watchdog that fires `mqtt.device_stale` if the device goes silent longer than `heartbeat_sec` seconds (and `mqtt.device_recovered` when it reports again).
   - Battery-powered / deep-sleep devices connecting with `cleanSession = false` automatically persist their subscriptions and receive up to 100 queued QoS 1 commands sent while they were asleep.
3. **Wire ingest rules:** `manage_mqtt add_rule { name: "ingest", topic_filter: "sensors/{zone}/{metric}", target_table: "readings", function_path: "on-reading", min_interval_sec: 10, retention_days: 7, rollup_interval: "1h" }`.
   - `topic_filter` supports standard MQTT wildcards (`+`, `#`) and named single-level captures like `{zone}`, `{device}`, `{metric}` (e.g. `zigbee2mqtt/{device}` or `tele/{device}/SENSOR`).
   - `target_table` matches JSON object keys and captured `{name}` segments (case-insensitively) to existing columns in the table and auto-fills `topic`, `device`, `payload` (and `value` for scalar/non-JSON payloads) whenever those columns exist.
   - `min_interval_sec` throttles raw table inserts per topic; `retention_days` auto-prunes old rows using `created_at`/`recorded_at`/`timestamp`/`ts`.
   - `rollup_interval` (`1m`, `5m`, `15m`, `1h`, `1d`) incrementally aggregates every numeric field into `min/max/avg/count/last` time-series buckets queryable via `GET /api/__mqtt/rollups` (and `manage_mqtt rollups`).
   - `function_path` invokes a `manage_function` handler with `req = { method: "MQTT", path, topic, payload, json, params, client_id, device }`.
4. **Bridge existing LAN brokers (optional):** `manage_mqtt add_bridge { name: "home_mosquitto", broker_url: "tcp://192.168.1.50:1883", remote_sub: "zigbee2mqtt/#,frigate/events", local_prefix: "lan/", local_pub: "lan/zigbee2mqtt/+/set", qos: 1 }`.
   - Connects outbound over `tcp://`/`mqtt://` or `tls://`/`mqtts://` to existing Mosquitto / EMQX brokers running **Zigbee2MQTT** (`zigbee2mqtt/#`), **ESPHome** (`esphome/#`), **Shelly** (`shelly*/#`), **Frigate NVR** (`frigate/events`), **Tasmota** (`tele/+/SENSOR`), or **Meshtastic** (`msh/+/2/json/#`).
   - Built-in loop prevention ensures messages injected from a bridge are never echoed back to it; `local_prefix` is automatically stripped on matching outbound `local_pub` topics before applying `remote_prefix`.
5. **React to presence & messages:** Broker events `mqtt.publish`, `mqtt.client_connected`, and `mqtt.client_disconnected` are emitted on the project event bus (usable with `manage_triggers`), and `manage_mqtt status` reports live connected clients, bridge health, and device `last_seen_at`.
6. **Publish from server functions:** inside any `manage_function` handler, call `ctx.mqtt.publish(topic, payload, { qos: 1, retain: true })` to send commands back to devices (or across outbound bridges).
7. **Test without hardware:** use `manage_mqtt publish` during `BUILD` to simulate incoming sensor payloads and verify that SQLite rows, triggers, and dashboards update.
