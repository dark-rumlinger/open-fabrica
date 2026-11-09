---
slug: inbound_webhooks
role: capability
title: Inbound webhooks
summary: Receive HMAC-signed webhooks from an external service (GitHub, a form host; not Stripe's own format) - the /webhooks/<name> route, the signature the platform verifies for you, and reacting via a webhook.received trigger.
category: automation
position: 41
requires: [event_actions]
tools: [manage_webhooks]
---

# Inbound webhooks (receiving external events)

An inbound webhook is an endpoint an OUTSIDE service calls to notify your app ("a payment cleared", "a commit was pushed"). Create one with `manage_webhooks` **create, omitting `url`** (a `url` makes it outbound instead). The tool returns a `secret` - the shared key the sender signs with.

## The route and the signature the platform checks for you

The endpoint is **`POST /webhooks/<name>`** - at the site root, NOT under `/api`, where `<name>` is the webhook's name. The platform **verifies an HMAC-SHA256 signature itself before your app ever sees the payload** - you do not write, and must not hand-roll, verification code. The caller must send:

```
X-Webhook-Signature: sha256=<hex HMAC-SHA256 of the raw request body, keyed with the secret>
```

(GitHub's `X-Hub-Signature-256` header is also accepted.) **Only that raw-body HMAC scheme is understood**: Stripe's `Stripe-Signature` never verifies here (Stripe payments use `/payments/webhook`, see the `payments` guide). A request with a missing or wrong signature is rejected `401`, an unknown name is `404`, a disabled webhook is `403`; only a correctly-signed body is accepted (`{ success: true }`) and goes on to fire an event. A sender that also sends `X-Webhook-Timestamp` must sign `<timestamp>.<body>` within 5 minutes; the body cap is 1 MB. So configuring the sender is two facts: point it at `https://<your-domain>/webhooks/<name>`, and give it the `secret` to sign with.

## React with a `webhook.received` trigger, never a handler

There is no page and no server route to write. A verified inbound webhook publishes the platform's **`webhook.received`** event carrying the received body; you act on it with a server-side trigger (`manage_triggers`) exactly like any other event - send an email, write a row, call an LLM. The event catalogue and the action types are the `event_actions` guide's job; this guide only gets the payload to the door. So the shape of an inbound integration is: `manage_webhooks create` (no `url`) -> hand the sender the URL + `secret` -> a trigger on `webhook.received` that does the work.

<!-- extended -->

## Inbound vs outbound - don't confuse them
> when: the app should CALL an external URL when something happens locally.

Outbound is the mirror image: `manage_webhooks create` **with** a `url`, then `subscribe` it to event types (`data.insert`, `payment.completed`, …). The platform then POSTs an HMAC-signed JSON body to that URL whenever a subscribed event fires - it signs with `X-Webhook-Signature` the same way it verifies inbound. The delivery is an envelope `{event, project_id, payload, timestamp}`, tried up to 3 times with backoff. The `trigger_webhook` trigger action is NOT the same thing: it posts the raw event payload (or its `payload` config) once with no retry. So if you're reacting to a local event by notifying an outside service, reach for the outbound webhook (or the `event_actions` webhook action), and keep this inbound guide for the receive direction only.

## Verifying wiring before go-live
> when: you want to confirm a trigger fires without the real sender.

`manage_triggers action=test` dry-runs your `webhook.received` trigger against a synthetic payload you supply, so you can confirm it does the right thing before the external service is connected. (`manage_webhooks test` is outbound-only - it POSTs to a webhook's own `url` - so it does not exercise an inbound receiver.)
