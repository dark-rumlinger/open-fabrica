---
id: trust-boundary
title: Trust boundary contract
kind: prose
governs: []
probes: [anyone can call]
---
**Trust boundary (any project with a server API).** Anything a page can call, anyone can call: a check only page code runs (a passcode screen, a hidden button, a role in `localStorage`) protects nothing. Protect writes on the server - an auth role on the endpoint, or a server function that verifies a token. Never invent a credential the owner must later know: ask for it with a `secret` question.
