package api

import "openfabrica/tools"

// manage_webhooks is the lifecycle surface for both incoming and outgoing
// webhooks. Handlers live in webhooks_handlers.go; this file is the schema.
//
// One arg struct per action - most actions only need a name; create/update
// carry the full target shape, subscribe owns event_types, test owns
// request_body.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_webhooks",
		Category: "api",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Define inbound and outbound webhook endpoints with event-type subscriptions.",
		WhenToUse: `Use create for an inbound webhook (omit url) or an outbound webhook (url
required). subscribe binds outbound webhooks to event types; the platform
fires them automatically on each event. test sends a synthetic payload to
verify wiring before going live.`,
		Actions: []tools.Action{
			&WebhookCreate{},
			&WebhookGet{},
			&WebhookList{},
			&WebhookDelete{},
			&WebhookUpdate{},
			&WebhookSubscribe{},
			&WebhookTest{},
		},
	})
}

// --- create -----------------------------------------------------------------

type WebhookCreate struct {
	Name string `arg:"name,required" desc:"Webhook name (also the route path for inbound)."`
	URL  string `arg:"url"           desc:"Target URL for outbound. Omit for inbound. Outbound calls are always POST with an HMAC-signed JSON body."`
}

func (a *WebhookCreate) Doc() string { return "Create an inbound or outbound webhook." }
func (a *WebhookCreate) Example() any {
	return &WebhookCreate{Name: "stripe_in"}
}
func (a *WebhookCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webhookCreate(ctx, a.Name, a.URL)
}

// --- get --------------------------------------------------------------------

type WebhookGet struct {
	Name string `arg:"name,required" desc:"Webhook name."`
}

func (a *WebhookGet) Doc() string    { return "Fetch one webhook's record." }
func (a *WebhookGet) ReadOnly() bool { return true }
func (a *WebhookGet) Example() any {
	return &WebhookGet{Name: "stripe_in"}
}
func (a *WebhookGet) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webhookGet(ctx, a.Name)
}

// --- list -------------------------------------------------------------------

type WebhookList struct{}

func (a *WebhookList) Doc() string    { return "List every webhook." }
func (a *WebhookList) ReadOnly() bool { return true }
func (a *WebhookList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webhookList(ctx)
}

// --- delete -----------------------------------------------------------------

type WebhookDelete struct {
	Name string `arg:"name,required" desc:"Webhook name."`
}

func (a *WebhookDelete) Doc() string { return "Delete a webhook by name." }
func (a *WebhookDelete) Example() any {
	return &WebhookDelete{Name: "stripe_in"}
}
func (a *WebhookDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webhookDelete(ctx, a.Name)
}

// --- update -----------------------------------------------------------------

type WebhookUpdate struct {
	Name      string `arg:"name,required" desc:"Webhook name to update."`
	URL       string `arg:"url"           desc:"New target URL for outbound."`
	IsEnabled *bool  `arg:"is_enabled"    desc:"Enable or disable."`
}

func (a *WebhookUpdate) Doc() string {
	return "Update an existing webhook's url or enabled state. Event subscriptions are managed by the subscribe action."
}
func (a *WebhookUpdate) Example() any {
	enabled := false
	return &WebhookUpdate{Name: "stripe_in", IsEnabled: &enabled}
}
func (a *WebhookUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webhookUpdate(ctx, a.Name, a.URL, a.IsEnabled)
}

// --- subscribe --------------------------------------------------------------

type WebhookSubscribe struct {
	Name       string `arg:"name,required"        desc:"Outbound webhook name."`
	EventTypes []any  `arg:"event_types,required" desc:"Event types to fire on (e.g. data.insert, data.update)." items:"string"`
}

func (a *WebhookSubscribe) ActionName() string { return "subscribe" }
func (a *WebhookSubscribe) Doc() string {
	return "Bind an outbound webhook to one or more event types."
}
func (a *WebhookSubscribe) Example() any {
	return &WebhookSubscribe{
		Name: "audit_relay", EventTypes: []any{"data.insert", "data.update"},
	}
}
func (a *WebhookSubscribe) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webhookSubscribe(ctx, a.Name, a.EventTypes)
}

// --- test -------------------------------------------------------------------

type WebhookTest struct {
	Name        string `arg:"name,required" desc:"Webhook name."`
	RequestBody string `arg:"request_body"  desc:"Synthetic payload sent to the webhook."`
}

func (a *WebhookTest) Doc() string {
	return "Send a synthetic payload to an OUTGOING webhook to verify its wiring (inbound webhooks cannot be tested this way - post to their URL instead)."
}
func (a *WebhookTest) ReadOnly() bool { return false }
func (a *WebhookTest) Example() any {
	return &WebhookTest{Name: "audit_relay", RequestBody: `{"event":"ping"}`}
}
func (a *WebhookTest) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return webhookTest(ctx, a.Name, a.RequestBody)
}
