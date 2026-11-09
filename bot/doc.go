// Package bot relays build questions and answers to a user over external
// messaging transports (for example Telegram). It manages per-user session
// state, dispatches incoming messages and callbacks, and drives the
// question/answer and form flows. It is the off-platform conversation surface,
// distinct from package chat (in-app chat).
package bot
