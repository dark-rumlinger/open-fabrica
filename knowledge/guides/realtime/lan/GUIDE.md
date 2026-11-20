---
slug: lan
role: capability
title: Local network discovery, Wake-on-LAN, reachability, and folder drop-zones
summary: Pure-Go local-first toolkit for Wake-on-LAN (ctx.lan.wake), private-LAN TCP pings (ctx.lan.ping), mDNS/Bonjour discovery (manage_lan), and local drop-zone folder watchers (manage_watch).
use_when: The app is a home-lab control panel, PC/NAS Wake-on-LAN dashboard, local network monitor, LAN device finder, or a local folder drop-zone (receipts, CSV imports, document inbox).
not_when: External cloud API calls or public internet uptime monitors - use ctx.fetch instead.
evidence_gated: true
category: realtime
position: 36
tools: [manage_lan, manage_watch]
---

# Local Network & Folder Drop-Zone Toolkit (`manage_lan`, `ctx.lan`, `manage_watch`)

Open Fabrica includes a pure-Go, local-first toolkit for home-lab, smart-home, and desktop automation apps. All network reachability checks are restricted to private RFC 1918 / loopback / `.local` hosts (cloud metadata and public internet targets are blocked), and folder watchers reject symlinks and protected system/credential paths.

## Tool Actions (`manage_lan`)

1. **Discover LAN services (mDNS/Bonjour):** `manage_lan discover { service: "_mqtt._tcp.local.", timeout_ms: 1500 }`
   - Useful service types: `_mqtt._tcp.local.` (Mosquitto/EMQX), `_home-assistant._tcp.local.`, `_esphomelib._tcp.local.`, `_http._tcp.local.`, `_smb._tcp.local.`, or `_services._dns-sd._udp.local.` to list every advertised service type on the LAN.
   - Pairs directly with `manage_mqtt add_bridge` to connect to a discovered LAN broker without hardcoding IPs.
2. **Wake a sleeping PC, NAS, or server:** `manage_lan wake { mac: "AA:BB:CC:DD:EE:FF", broadcast: "255.255.255.255", port: 9 }`.
3. **Check LAN port reachability:** `manage_lan ping { host: "192.168.1.50", port: 1883, timeout_ms: 1000 }`.

## Server Function Runtime (`ctx.lan`)

Inside any `manage_function` handler:
- `ctx.lan.wake(mac, { broadcast?, port? })` → `{ ok, mac, broadcast, port }` — sends a Wake-on-LAN magic packet when a user clicks "Wake PC" in the app UI.
- `ctx.lan.ping(host, port, timeoutMs?)` → `{ reachable, latency_ms, host, port }` — checks whether a NAS, server, printer, or broker on the LAN is online (max 2000ms timeout, private LAN / `.local` hosts only).

## Local Folder Drop-Zone Watcher (`manage_watch`)

1. **Register a drop-zone:** `manage_watch add { name: "csv_imports", glob: "*.csv,*.json", function_path: "on-file-drop", poll_sec: 5 }`.
   - Leave `folder` blank to auto-create the project's drop-zone directory (`<project>/watch/<name>`) and display its path to the user.
   - When a matching file is dropped or updated, Open Fabrica deduplicates it via SQLite, emits a `folder.file_dropped` event (usable with `manage_triggers`), and invokes `function_path` with `req = { method: "WATCH", watcher, folder, name, ext, size_bytes, mime_type, is_text, content }` (UTF-8 text up to 256 KiB).
2. **Inspect or remove:** `manage_watch list` / `manage_watch delete { name: "csv_imports" }`.
