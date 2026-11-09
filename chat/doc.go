// Package chat handles in-app chat sessions with the build engine: it persists the
// owner's messages, hands them to the engine (which answers), and loads message
// history and the timeline from the project database. It is the in-app conversation
// surface, distinct from package bot (external messaging transports).
package chat
