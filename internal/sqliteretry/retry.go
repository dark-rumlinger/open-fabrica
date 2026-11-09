// Package sqliteretry wraps SQLite calls with bounded, jittered retries on
// BUSY/locked errors, counting events so real contention stays observable.
package sqliteretry

import (
	"context"
	"database/sql"
	"log/slog"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"openfabrica/internal/stringutil"
)

const maxRetries = 5

// BUSY events are rare under the current DB config (busy_timeout=30s + single
// writer pool). If they start showing up in logs or these counters climb, the
// retry wrapper is doing real work and the underlying contention is worth
// investigating before trimming it.
var (
	busyRetries   atomic.Uint64
	busyExhausted atomic.Uint64
)

// Stats returns the cumulative count of (retried, exhausted) BUSY events since
// process start. Surfacing these via an admin endpoint or metric is what tells
// you whether this layer is theatre or load-bearing.
func Stats() (retried, exhausted uint64) {
	return busyRetries.Load(), busyExhausted.Load()
}

// isBusy returns true if the error is a SQLite BUSY/locked error.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// retryBusy runs op, retrying on SQLite BUSY/locked with jittered backoff. ctx
// cancels the backoff wait; label/query only flavor the logs. It is the single
// loop every public wrapper shares.
func retryBusy[T any](ctx context.Context, label, query string, op func() (T, error)) (T, error) {
	baseDelay := 5 * time.Millisecond
	for attempt := 0; ; attempt++ {
		res, err := op()
		if err == nil || !isBusy(err) {
			return res, err
		}
		if attempt >= maxRetries {
			busyExhausted.Add(1)
			slog.Error("sqlite busy retries exhausted", "op", label, "attempts", attempt+1, "query", truncateSQL(query), "err", err)
			return res, err
		}
		busyRetries.Add(1)
		delay := backoff(baseDelay, attempt)
		slog.Warn("sqlite busy, retrying", "op", label, "attempt", attempt+1, "delay", delay)
		if werr := wait(ctx, delay); werr != nil {
			return res, werr
		}
	}
}

// Exec wraps db.Exec with retry-on-busy logic (exponential backoff).
func Exec(db *sql.DB, query string, args ...any) (sql.Result, error) {
	return ExecContext(context.Background(), db, query, args...)
}

// ExecContext is Exec bound to ctx: ctx cancels both the statement and any
// backoff wait, so the call cannot outlive its caller.
func ExecContext(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
	return retryBusy(ctx, "write", query, func() (sql.Result, error) {
		return db.ExecContext(ctx, query, args...)
	})
}

// Execer is the write side of a database handle: any type that can run a
// statement. Both *sql.DB and a routed pool handle satisfy it.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// ExecHandle wraps h.Exec with the same retry-on-busy logic as Exec, for callers
// that hold a pool handle rather than a concrete *sql.DB. The handle has no ctx,
// so the backoff wait runs under Background (not cancellable); reserve it for
// short best-effort writes.
func ExecHandle(h Execer, query string, args ...any) (sql.Result, error) {
	return retryBusy(context.Background(), "write", query, func() (sql.Result, error) {
		return h.Exec(query, args...)
	})
}

// BeginTx wraps db.Begin with retry-on-busy logic.
func BeginTx(db *sql.DB) (*sql.Tx, error) {
	return BeginTxContext(context.Background(), db)
}

// BeginTxContext is BeginTx bound to ctx. Note that ctx also governs the
// returned transaction: database/sql rolls it back when ctx ends.
func BeginTxContext(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return retryBusy(ctx, "BEGIN", "", func() (*sql.Tx, error) {
		return db.BeginTx(ctx, nil)
	})
}

// Query wraps db.Query with retry-on-busy logic (exponential backoff).
func Query(db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	return QueryContext(context.Background(), db, query, args...)
}

// QueryContext is Query bound to ctx: ctx cancels both the statement and any
// backoff wait, so the call cannot outlive its caller.
func QueryContext(ctx context.Context, db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	return retryBusy(ctx, "read", query, func() (*sql.Rows, error) {
		return db.QueryContext(ctx, query, args...)
	})
}

// wait sleeps for d, or returns ctx's error if ctx ends first. Backing off
// without it would let a retry loop keep a caller past its deadline.
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func truncateSQL(q string) string {
	return stringutil.Truncate(strings.TrimSpace(q), 200)
}

func backoff(base time.Duration, attempt int) time.Duration {
	delay := base * (1 << attempt) // 5ms, 10ms, 20ms, 40ms, 80ms
	jitter := time.Duration(rand.Int63n(int64(delay/2) + 1))
	return delay + jitter
}
