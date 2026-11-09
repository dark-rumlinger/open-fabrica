// Package tools is the registry and implementations of the actions the model
// invokes during a build. Tools self-register via init() in category subpackages
// (api, content, data, project, auth, payments, messaging, observability,
// automation), and the registry validates a model-supplied argument map and
// dispatches it to the matching handler. Handlers return outcomes as Result data
// with a nil Go error so the model can read and react to failures.
package tools
