package llm

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync/atomic"

	"openfabrica/internal/safego"
	"openfabrica/internal/sqliteretry"
)

const logBufferSize = 256

// LogRedactor scrubs sensitive material from a log entry in place, just before
// it is persisted to of_llm_calls. It runs in the logger's writer goroutine,
// off the caller's hot path. This package defines only the contract: the
// implementation knows which tool arguments and table columns are sensitive and
// is injected by the engine/chat wiring via WithRedactor, so the llm package
// stays free of project-schema knowledge.
type LogRedactor interface {
	Redact(*LLMLogEntry)
}

// DBLLMLogger writes LLM log entries to a project's SQLite database
// through a buffered channel and single writer goroutine.
type DBLLMLogger struct {
	db       *sql.DB
	ch       chan LLMLogEntry
	done     chan struct{}
	redactor LogRedactor
	// onLogged, when set, is told about each row after it is written, with the
	// row id. It runs on the writer goroutine, so it must return quickly.
	onLogged func(entry LLMLogEntry, rowID int64)
	// dropped counts entries the full buffer discarded. A dropped row is
	// un-billed spend against the daily ceiling and a hole in the cost views,
	// so the total is surfaced once at Close instead of one Warn per drop.
	dropped atomic.Int64
}

// Option configures a DBLLMLogger at construction.
type Option func(*DBLLMLogger)

// WithRedactor attaches a redactor applied to every entry before it is written.
// Without it the logger persists entries verbatim.
func WithRedactor(r LogRedactor) Option {
	return func(l *DBLLMLogger) { l.redactor = r }
}

// WithOnLogged registers a callback fired after each row is stored. The engine
// uses it to announce LLM calls to live viewers.
func WithOnLogged(fn func(entry LLMLogEntry, rowID int64)) Option {
	return func(l *DBLLMLogger) { l.onLogged = fn }
}

// NewDBLLMLogger creates a logger that writes to the given project database.
// Call Close() when done to drain the buffer.
func NewDBLLMLogger(projectDB *sql.DB, opts ...Option) *DBLLMLogger {
	l := &DBLLMLogger{
		db:   projectDB,
		ch:   make(chan LLMLogEntry, logBufferSize),
		done: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}
	safego.Go("llm-dblogger", l.writeLoop)
	return l
}

// LogLLMCall queues a log entry for async writing.
func (l *DBLLMLogger) LogLLMCall(entry LLMLogEntry) {
	select {
	case l.ch <- entry:
	default:
		if l.dropped.Add(1) == 1 {
			slog.Warn("llm log buffer full, dropping entries (total reported at close)", "source", entry.Source)
		}
	}
}

// Close drains remaining entries and stops the writer goroutine.
func (l *DBLLMLogger) Close() {
	close(l.ch)
	<-l.done
	if n := l.dropped.Load(); n > 0 {
		slog.Warn("llm log dropped entries; token accounting is under-counted", "dropped", n)
	}
}

// chainTail is what the writer remembers about the last row it wrote for one chain, so
// the next row can be stored as a delta against it. Only fingerprints are kept, 8 bytes per
// message, so remembering a 200-message transcript costs about 1.6 KB.
type chainTail struct {
	rowID    int64
	hashes   []uint64
	sysHash  uint64
	toolHash uint64
	seq      uint64 // write order, for evicting the least recently written chain
}

// maxTrackedChains bounds the writer's delta state. A project runs a handful of chains
// at once (the build loop, owner chat, a trigger); past that the oldest is evicted and its
// next row is simply stored self-contained, which is correct, just larger.
const maxTrackedChains = 16

func (l *DBLLMLogger) writeLoop() {
	defer close(l.done)
	tails := map[string]*chainTail{}
	var seq uint64
	for entry := range l.ch {
		if l.redactor != nil {
			l.redactor.Redact(&entry)
		}
		cacheHit := boolToInt(entry.CacheHit)
		seq++
		chain := CallChain{Source: entry.Source, RunID: entry.RunID, SessionID: entry.SessionID}
		stored, storedSys, storedTools, tail := l.encodeRequest(&entry, chain, tails, seq)
		res, err := sqliteretry.Exec(l.db,
			`INSERT INTO of_llm_calls (
				source, run_id, session_id, stage, iteration, model, provider_type, provider_name,
				request_hash, cache_hit,
				request_messages, request_system, request_tools, request_max_tokens,
				response_content, response_tool_calls, response_stop_reason,
				input_tokens, output_tokens,
				cache_read_input_tokens, cache_creation_input_tokens,
				stable_prefix_bytes, stable_prefix_msgs, total_request_bytes,
				request_tool_bytes, tool_prefix_changed,
				duration_ms, attempts, retry_wait_ms, used_fallback, error_message
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.Source, entry.RunID, entry.SessionID, entry.Stage, entry.Iteration,
			entry.Model, entry.ProviderType, entry.ProviderName,
			entry.RequestHash, cacheHit,
			stored, storedSys,
			storedTools, entry.RequestMaxTokens,
			entry.ResponseContent, entry.ResponseToolCalls, entry.ResponseStopReason,
			entry.InputTokens, entry.OutputTokens,
			entry.CacheReadInputTokens, entry.CacheCreationInputTokens,
			entry.StablePrefixBytes, entry.StablePrefixMsgs, entry.TotalRequestBytes,
			entry.RequestToolBytes, boolToInt(entry.ToolPrefixChanged),
			entry.DurationMs, entry.Attempts, entry.RetryWaitMs, boolToInt(entry.UsedFallback),
			nullIfEmpty(entry.ErrorMessage),
		)
		if err != nil {
			slog.Debug("llm log write failed", "source", entry.Source, "error", err)
			// The row this session's next delta would have pointed at does not exist,
			// so forget the session and store the next one whole.
			delete(tails, chain.Key())
			continue
		}
		id, err := res.LastInsertId()
		if err == nil && l.onLogged != nil {
			l.onLogged(entry, id)
		}
		if tail == nil {
			continue
		}
		if err != nil {
			delete(tails, chain.Key())
			continue
		}
		tail.rowID = id
		tails[chain.Key()] = tail
		evictOldestChains(tails)
	}
}

// encodeRequest decides how this row stores its request. When the session's previous row
// shares a leading run of messages, only the messages after that run are stored, along with
// the base row id and the shared count; the system prompt and tool list are dropped when
// they match the base, which for a build session is every row after the first. Returns the
// three column values plus the tail state to record once the row has an id (nil when the
// row is not trackable, so nothing is remembered for it).
func (l *DBLLMLogger) encodeRequest(entry *LLMLogEntry, chain CallChain, tails map[string]*chainTail, seq uint64) (msgs, system, tools string, next *chainTail) {
	system, tools = entry.RequestSystem, entry.RequestTools
	if chain.Key() == "" || entry.RequestMessages == "" {
		return entry.RequestMessages, system, tools, nil
	}
	var parsed []json.RawMessage
	if err := json.Unmarshal([]byte(entry.RequestMessages), &parsed); err != nil {
		return entry.RequestMessages, system, tools, nil
	}

	next = &chainTail{
		hashes:   messageHashes(parsed),
		sysHash:  contentHash([]byte(system)),
		toolHash: contentHash([]byte(tools)),
		seq:      seq,
	}

	prev := tails[chain.Key()]
	if prev == nil || prev.rowID == 0 {
		return entry.RequestMessages, system, tools, next
	}
	shared := commonPrefixLen(prev.hashes, next.hashes)
	if shared == 0 {
		return entry.RequestMessages, system, tools, next
	}

	d := requestDelta{Base: prev.rowID, Prefix: shared, Msgs: parsed[shared:]}
	if next.sysHash != prev.sysHash {
		d.System = &system
	}
	if next.toolHash != prev.toolHash {
		d.Tools = &tools
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return entry.RequestMessages, system, tools, next
	}
	// A delta that is not actually smaller (a short transcript, a near-total rewrite) is
	// not worth the replay hop.
	if len(encoded) >= len(entry.RequestMessages) {
		return entry.RequestMessages, system, tools, next
	}
	storedSys, storedTools := "", ""
	if d.System != nil {
		storedSys = system
	}
	if d.Tools != nil {
		storedTools = tools
	}
	return string(encoded), storedSys, storedTools, next
}

// evictOldestChains keeps the delta state bounded. Dropping a chain only costs the next
// row of it being stored whole.
func evictOldestChains(tails map[string]*chainTail) {
	for len(tails) > maxTrackedChains {
		oldest, oldestSeq := "", ^uint64(0)
		for k, v := range tails {
			if v.seq < oldestSeq {
				oldest, oldestSeq = k, v.seq
			}
		}
		delete(tails, oldest)
	}
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// boolToInt renders a flag for a SQLite integer column.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
