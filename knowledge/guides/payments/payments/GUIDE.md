---
slug: payments
role: capability
title: Payments and checkout
summary: Hosted checkout + fulfilment on the payment.completed webhook (never the redirect); one-off and subscriptions.
use_when: The project takes money - checkout, subscriptions, a shop, selling anything, paid plans, pricing tiers, or donations.
not_when: A project that only displays prices as content, or has no transaction of any kind.
evidence_gated: true
category: payments
position: 45
tools: [manage_payments, manage_subscriptions, manage_subscription_plans]
---

# Payments (any project that takes money)

Selling something - a purchase, a paid plan, a donation. Checkout is hosted by the provider (Stripe, PayPal, Mollie; only Stripe reconciles automatically), so no card data reaches the app.

## The contract

{{contract:payment-flow}}

## Wiring it, in order

1. **Credentials first.** One `manage_chat ask` (`question_type: "form"`, `fields`: two `{name, label, type: "secret", secret_name}` entries) for the provider's API key and webhook signing secret; `manage_providers add` the provider's API with that key and `base_url` `https://api.stripe.com` (no `/v1`: the tools append it); then `manage_payments configure` (`provider_name`, `provider_type` - `stripe` by default, the only family reconciled automatically - `currency`, `webhook_secret_name`), which refuses without both.
2. **Checkout** is the contract's `manage_function` handler: validate the order, `ctx.checkout`, return `checkout_url` for the page to redirect to. Never collect card numbers yourself.
3. **Fulfilment** is a `manage_triggers` hook on `payment.completed` (Decision below): grant the role / flip the order row / email the receipt, correlating by `external_id` or `metadata`.
4. **Pages.** `success_url` and `cancel_url` are real page routes (`/thank-you`, `/cart`); the success page shows a pending state and reflects the order once the webhook lands - it grants nothing.

## Decision: fulfillment authority (record in the plan's `decisions`)

Where the purchased thing is granted is load-bearing (the contract above says why). Record `topic: "fulfillment authority"`:

- **Server-side on the payment webhook** (the only correct choice) -> `choice: "webhook-trigger"`. Plan a `manage_triggers` task on `event_type:"payment.completed"` that does the fulfilment. `check: {artifact:"trigger", id:"payment.completed", field:"count", op:"gt", value:0}` - the build is held to it, so a redirect-only grant is sent back for repair.

<!-- extended -->

## Subscriptions (recurring)
> when: the brief is a recurring plan / membership, not a one-off purchase.

Define the tiers with `manage_subscription_plans` (`create` / `list`), then `manage_subscriptions` (`create` opens the recurring hosted checkout, `list`, `cancel`, `check`). Subscriptions need a Stripe provider: the plan tools refuse any other, because only Stripe subscription events are reconciled. Gate access by a status column your fulfilment trigger maintains and the app reads (`manage_subscriptions check` is a BUILD-time tool: it is absent from the running app, and an app function's `ctx.query` cannot read the system `of_subscriptions` table), not by a one-time flag. `cancel` ends the subscription immediately; there is no cancel-at-period-end, so say so in the UI rather than promising access until the period closes. `check` also rewrites the local status from the provider without raising an event. The same fulfilment rule holds: the source of truth for "is this subscription active" is the provider webhook, reconciled server-side, never a client claim. Going active or renewing raises `payment.completed`; failing or cancelling raises `payment.failed` (payload has `subscription_id`).

## Reconciliation and testing
> when: confirming a payment landed, or a webhook was missed.

The success page's "confirmed yet?" check reads the app's OWN order row (flipped by your `payment.completed` trigger) over its `create_api`; the payments tools are absent from the running app. `manage_payments check_status` reads the local payment row (`pending` until the signed webhook lands, then `paid`, `failed` or `expired`) for diagnosis only: it does not ask the provider, so a missed webhook is redelivered from the provider's dashboard. `manage_payments list` shows recorded payments (filter `status=paid`). Only Stripe is reconciled automatically: PayPal, Mollie and `generic` payments stay `pending`, so `payment.completed` never fires for them. Never treat a checkout as paid until the row says `paid`.
