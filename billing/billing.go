// Package billing is the payments domain shared by the tool that configures
// payments (tools/payments) and the public server that receives provider
// callbacks (server/public): provider webhook signature verification and the
// reconciliation of money state a verified webhook implies. Its tables
// (of_payment_config, of_payments, of_processed_webhooks, of_subscriptions) are in the
// project migration. Keeping it outside both callers is what guarantees there is
// exactly one gate between an incoming webhook and a money row.
package billing
