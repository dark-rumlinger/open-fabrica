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

## Local Folder Drop-Zone Watcher (`manage_watch` & `ctx.watch`)

1. **Register a watched folder:** `manage_watch add { name: "dropwatch", glob: "*", function_path: "watch/events", poll_sec: 2 }`.
   - Leave `folder` blank to auto-create the project's drop-zone directory (`<project>/watch/<name>`) and surface its path in the tool response.
   - Open Fabrica runs the watcher loop in Go (including subfolders), deduplicates file versions in SQLite, emits a `folder.file_dropped` event (usable with `manage_triggers`), and invokes `function_path` **once per created, modified, or deleted file** with:
     `req = { method: "WATCH", watcher, folder, event: "created"|"modified"|"deleted", kind: "created"|"changed"|"deleted", path, rel_path, name, ext, size_bytes, size, mtime, mime_type, is_text, content, body: { ...same fields... } }` (`content` carries UTF-8 text up to 256 KiB when `is_text` is true).
2. **Read live folder inventory inside any server function (`ctx.watch.list`):**
   - `ctx.watch.list(watcherName?)` → `{ ok, available, watcher, folder, path, count, files: [{ path, name, ext, size_bytes, size, mtime, mtime_ms, is_dir }] }`.
   - Use `ctx.watch.list()` in a `GET` state endpoint (or startup sync) to read the current baseline directory contents without needing a scheduled polling task.
3. **Example `function_path` handler (`watch/events`):**
   ```js
   function handler(req, ctx) {
     const ev = req.body || req;
     if (!ev || !ev.name) return { ok: false };
     const path = ev.path || ev.rel_path || ev.name;
     const kind = ev.kind || (ev.event === 'modified' ? 'changed' : (ev.event || 'created'));
     const nowIso = ev.mtime || new Date(ctx.now()).toISOString();
     ctx.insert('watch_events', { kind, name: ev.name, path, size: ev.size_bytes || 0, mtime: nowIso, ts: nowIso });
     if (kind === 'deleted') {
       ctx.delete('watch_files', { path });
     } else {
       ctx.upsert('watch_files', { path, name: ev.name, size: ev.size_bytes || 0, mtime: nowIso, is_dir: 0 }, ['path']);
     }
     return { ok: true, kind, path };
   }
   ```
   - When `watch_events` is backed by a `manage_api` endpoint and a `manage_sse` stream, `ctx.insert('watch_events', ...)` automatically broadcasts a `data.insert` SSE event to connected browsers!
4. **Inspect or remove:** `manage_watch list` / `manage_watch delete { name: "dropwatch" }`.
