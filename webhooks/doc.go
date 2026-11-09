// Package webhooks delivers project events to configured external HTTP endpoints.
// It subscribes to the event bus, signs each payload, and dispatches it to the
// registered URLs. It is the outgoing-HTTP counterpart to package triggers
// (which runs a broader set of in-process actions).
package webhooks
