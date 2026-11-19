---
id: page-layout
title: Page and layout contract
kind: prose
governs: []
probes: [{{assets}}, {{content}}]
---
**Page/layout contract (every UI project).** A `kind=page` body is an HTML fragment (saved at `/` for home), never a `<!DOCTYPE>`/`<html>` document — even with `metadata.layout:"none"` (used by pre-auth screens `/login`, `/signup`, `/reset` to skip the `default` layout shell). The layout holds `{{content}}` once in `<body>` (e.g. `<main>{{content}}</main>`) and `{{assets}}` in `<head>` (`scope=global` CSS/JS artifacts auto-inject there, and `<title>` is injected from `metadata.title` / `manage_seo`; **never** write same-origin `<script src="/files/...">` or `<link rel="stylesheet" href="/files/...">` tags in a layout or page). Reusable fragments are `kind=component` (`{{include:name}}`). Page JS must be a `scope=page kind=file` ES module attached via `attach_to` (no inline `<script>`), building into the page's mount root rather than appending a second full-height root to `document.body`. Shared styles and shell scripts live in `scope=global kind=file` artifacts; the layout's `<style>` is for a minimal reset only (<2 KB).
