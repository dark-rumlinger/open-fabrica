// Package triggers runs server-side actions in response to project events. It
// subscribes to the event bus and executes configured actions (email, HTTP
// request, data mutation, or a project function), recording an audit trail. It
// is one of several event-driven mechanisms; outgoing HTTP-only delivery lives
// in package webhooks.
package triggers
