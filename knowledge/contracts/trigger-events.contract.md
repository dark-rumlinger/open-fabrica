---
id: trigger-events
title: Server-side trigger event catalogue
kind: kv
governs: [event_actions]
probes: [payment.completed, data.insert]
---
**Event catalogue for server-side triggers (`manage_triggers`, the `event_type` field).** A trigger fires server-side when a matching event is published; `{{field}}` placeholders in its config expand against the event PAYLOAD. Matching is exact string equality on the name, so a misspelled `event_type` never matches and the trigger silently never runs.

These are the events the PLATFORM publishes on its own:

| event_type | fires when | payload fields (`{{...}}`) |
|---|---|---|
| `data.insert` | a row is created (a `create_api` POST, or a PUT on a natural key that creates the row) | `table`, `endpoint`, `id`, + the saved row's visible columns (its `public_columns`; PASSWORD/ENCRYPTED stripped) |
| `data.update` | an existing row is updated (PUT) | `table`, `endpoint`, `id`, + the whole updated row's visible columns (not only the changed ones) |
| `data.delete` | a row is deleted (DELETE) | `table`, `endpoint`, `id` |
| `auth.register` | an account is created (`create_auth` register, or a first OAuth / magic-link sign-in) | `user_id`, `table`, `method` (`password`/`oauth`/`magic_link`), + non-secret registration fields (e.g. `email`) |
| `auth.login` | a user signs in by any method | `user_id`, `table`, `role`, `method`, + the username column |
| `webhook.received` | an inbound webhook arrives (`manage_webhooks`) | `webhook_id`, `webhook_name`, + every top-level field of the received webhook body |
| `notification.created` | `manage_notifications` sends one (a trigger's `send_notification` does not raise it) | `title`, `type`, `count` (recipients reached). There is NO `user_id`: one call fans out to many users, so the event reports the fan-out, not a recipient. |
| `tenant.created` / `tenant.member_invited` / `tenant.member_joined` / `tenant.switched` | multi-tenancy lifecycle (`manage_tenancy`, tenant signup) | created: `tenant_id`, `slug`, `name`, `plan`, `owner_user_id` (0 from the agent); member_invited: `tenant_id`, `slug`, `email`, `role`, `token`; member_joined: `tenant_id`, `user_id`, `role`; switched: `user_id`, `tenant_id`, `slug` |
| `payment.completed` | a signature-verified provider webhook reconciles a payment to `paid` (the platform's `POST /payments/webhook` route, or `manage_payments handle_webhook` for a payload you hold) | `external_id` (the provider's session/payment id - the correlation key), `status`, `amount`, `currency`, `description`, + every key of the checkout `metadata` you passed. A subscription-originated event carries `subscription_id`, `plan_id`, `user_id` and `status` instead of `amount`/`currency`/`description` |
| `payment.failed` | the same webhook reconciles a payment to a terminal non-paid status (failed/canceled/expired/...) | `external_id`, `status`, `reason`, `amount`, `currency`, `description`, + the checkout `metadata` |

**A CUSTOM event name is legitimate.** A `manage_schedules` task can publish any name it likes, and a trigger listening on that name fires normally - the catalogue above is what the platform raises for you, not a closed set the runner enforces. Nothing validates the pairing, so a name published in one place and listened for in another with a typo is a trigger that never runs and never complains.

`event_filter` (e.g. `{"table":"orders"}`) narrows WHICH payloads match - ALL of its keys must equal the payload. `timing` is always `after`: every trigger runs asynchronously once the event is published (the write that raised it has already committed). There is no synchronous pre-commit hook - validate or stamp derived fields in the code that writes the row, not a trigger.
