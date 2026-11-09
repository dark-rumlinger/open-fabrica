// Package safego launches background goroutines that cannot crash the process.
//
// An unrecovered panic in ANY goroutine terminates the whole program, so in
// this single-binary, multi-project server every background goroutine must be
// launched through here (router recover middleware covers handlers, not the
// goroutines they spawn). Use Go/Run for one-shot work and Supervise for
// long-lived loops that must stay alive.
package safego

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// Go runs fn in a new goroutine, recovering and logging any panic. name
// identifies the task in the recovery log.
func Go(name string, fn func()) {
	go Run(name, fn)
}

// Run executes fn in the current goroutine with panic recovery. Use it to wrap
// a goroutine body you launch yourself (e.g. inside a WaitGroup-tracked closure
// that must still run its deferred bookkeeping) so the recovery is in place
// without adding another goroutine layer.
func Run(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic in background task",
				"task", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// Recover is a deferred panic handler for a goroutine you launch by hand and
// whose body you cannot easily hand to Go/Run (e.g. it has an essential
// `defer cancel()` that must still run). Use as the FIRST defer so it executes
// before the others:
//
//	go func() {
//	    defer cancel()
//	    defer safego.Recover("ws-read-pump")
//	    ...
//	}()
func Recover(name string) {
	if r := recover(); r != nil {
		slog.Error("recovered from panic in background task",
			"task", name, "panic", r, "stack", string(debug.Stack()))
	}
}

// superviseBackoff is the pause between restarts of a supervised loop. Long
// enough that a tight panic-restart cycle cannot spin the CPU, short enough that
// a transient fault recovers quickly.
const superviseBackoff = time.Second

// Supervise runs fn (typically a long-lived loop) with panic recovery and
// restarts it until ctx is cancelled. A loop that returns normally on ctx.Done
// is not restarted. Use for always-on singletons (task scheduler, maintenance,
// the bot poller) whose silent death would disable a feature with no signal.
func Supervise(ctx context.Context, name string, fn func()) {
	go func() {
		for ctx.Err() == nil {
			Run(name, fn)
			if ctx.Err() != nil {
				return
			}
			slog.Warn("background loop exited, restarting", "task", name, "after", superviseBackoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(superviseBackoff):
			}
		}
	}()
}
