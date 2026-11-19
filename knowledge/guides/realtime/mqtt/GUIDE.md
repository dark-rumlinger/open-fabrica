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

1. **Enable the broker:** `manage_mqtt configure { enabled: true, allow_anonymous: false }`.
2. **Register devices:** `manage_mqtt add_device { name: "esp1", username: "esp1", allow_pub: "sensors/esp1/#", allow_sub: "actuators/esp1/#" }`. Save or show the generated `token` to the owner once.
3. **Wire ingest rules:** `manage_mqtt add_rule { name: "ingest", topic_filter: "sensors/+/reading", target_table: "readings", function_path: "on-reading" }`.
   - `target_table` auto-inserts JSON object fields (or `{topic, payload, device}` for non-JSON payloads) into a project SQLite table.
   - `function_path` invokes a `manage_function` handler with `req = { method: "MQTT", path, topic, payload, json, client_id, device }`.
4. **Publish from server functions:** inside any `manage_function` handler, call `ctx.mqtt.publish(topic, payload, { qos: 1, retain: true })` to send commands back to devices.
5. **Test without hardware:** use `manage_mqtt publish` during `BUILD` to simulate incoming sensor payloads and verify that SQLite rows, triggers, and dashboards update.
