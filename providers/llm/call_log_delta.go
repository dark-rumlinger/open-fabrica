package llm

import (
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"strings"
)

// Delta storage for of_llm_calls.request_messages.
//
// Every engine turn re-sends the whole conversation, and the log stored the whole thing
// again on every row, so one build's rows held roughly (turns x transcript) bytes, with
// the full system prompt repeated on every row.
//
// A row now stores EITHER a plain JSON array (self-contained: every row written before
// this existed, and the first row of every session) OR a requestDelta object naming the
// row it continues. Readers tell them apart by the first non-space byte, so no schema
// change and no migration is involved and old rows keep reading exactly as before.

// requestDelta is the stored shape of a request whose messages continue an earlier row in
// the same session. Base is that row's id; Prefix is how many messages of the base's
// RECONSTRUCTED array come before Msgs. System and Tools are carried only when they differ
// from the base, so an absent field means "same as the base", never "empty".
type requestDelta struct {
	Base   int64             `json:"base"`
	Prefix int               `json:"prefix"`
	Msgs   []json.RawMessage `json:"msgs"`
	System *string           `json:"system,omitempty"`
	Tools  *string           `json:"tools,omitempty"`
}

// isDeltaEncoded reports whether a stored request_messages value is a delta object rather
// than a self-contained array.
func isDeltaEncoded(stored string) bool {
	s := strings.TrimLeft(stored, " \t\r\n")
	return strings.HasPrefix(s, "{")
}

// contentHash fingerprints one stored blob. Used to compare a request against the previous
// one in the same session without keeping the previous request's bytes in memory.
func contentHash(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

func messageHashes(msgs []json.RawMessage) []uint64 {
	out := make([]uint64, len(msgs))
	for i, m := range msgs {
		out[i] = contentHash(m)
	}
	return out
}

// commonPrefixLen returns how many leading elements two hash lists share.
func commonPrefixLen(a, b []uint64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// CallChain identifies the sequence of logged calls a delta may chain along. Keyed on the
// RUN when there is one, so a segment rollover and a warm fix-pass both continue the
// previous segment's chain instead of restarting it: a fix-pass carries the whole prior
// transcript forward under a fresh session id, which would otherwise cost a full
// self-contained copy of it. Chat and proxy calls carry no run, so they chain by
// session. Source is part of the key so a non-engine call sharing a run can never
// become an engine call's base.
//
// A chain that resolves to nothing (no run and no session) disables delta encoding for that
// row, which is correct: there is no sequence to replay it against.
type CallChain struct {
	Source    string
	RunID     string
	SessionID string
}

// Key is the writer's per-chain bucket. Empty means this row stands alone.
func (c CallChain) Key() string {
	if c.RunID == "" && c.SessionID == "" {
		return ""
	}
	scope := c.RunID
	if scope == "" {
		scope = c.SessionID
	}
	return c.Source + "\x00" + scope
}

// filter returns the WHERE clause and argument that select exactly this chain's rows, in the
// same partition Key() buckets them into.
func (c CallChain) filter() (string, string) {
	if c.RunID != "" {
		return "source = ? AND run_id = ?", c.RunID
	}
	return "source = ? AND session_id = ?", c.SessionID
}

// RequestReplayer folds a chain's stored rows back into full request payloads. Rows must be
// fed in ascending id order within each chain, which is how both the export and the one-shot
// replay read them. Zero value is ready to use.
type RequestReplayer struct {
	state map[string]*replayState
}

type replayState struct {
	msgs   []json.RawMessage
	system string
	tools  string
}

// Next folds one row and returns the full request it stands for. A self-contained row
// replaces the chain's running state; a delta row extends it. Rows fed out of order, or a
// delta whose base was never seen, degrade to whatever the row itself carries rather than
// erroring: this serves diagnostics, and a partial transcript beats none.
func (r *RequestReplayer) Next(chain CallChain, stored, system, tools string) (msgs []json.RawMessage, sysOut, toolsOut string) {
	if r.state == nil {
		r.state = map[string]*replayState{}
	}
	key := chain.Key()
	st := r.state[key]
	if st == nil {
		st = &replayState{}
		r.state[key] = st
	}

	if !isDeltaEncoded(stored) {
		st.msgs = nil
		if stored != "" {
			if err := json.Unmarshal([]byte(stored), &st.msgs); err != nil {
				st.msgs = nil
			}
		}
		st.system, st.tools = system, tools
		return st.msgs, st.system, st.tools
	}

	var d requestDelta
	if err := json.Unmarshal([]byte(stored), &d); err != nil {
		return st.msgs, st.system, st.tools
	}
	prefix := d.Prefix
	if prefix > len(st.msgs) {
		prefix = len(st.msgs)
	}
	full := make([]json.RawMessage, 0, prefix+len(d.Msgs))
	full = append(full, st.msgs[:prefix]...)
	full = append(full, d.Msgs...)
	st.msgs = full
	if d.System != nil {
		st.system = *d.System
	}
	if d.Tools != nil {
		st.tools = *d.Tools
	}
	return st.msgs, st.system, st.tools
}

// AppendedRequestMessages returns only the messages a stored request_messages
// value newly carries: the delta's own msgs, or the whole array for a
// self-contained row. Nil when the value cannot be parsed. Lets a scanner walk
// a chain in id order and see every message exactly at its first appearance,
// before any later compaction rewrite.
func AppendedRequestMessages(stored string) []json.RawMessage {
	if stored == "" {
		return nil
	}
	if isDeltaEncoded(stored) {
		var d requestDelta
		if err := json.Unmarshal([]byte(stored), &d); err != nil {
			return nil
		}
		return d.Msgs
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal([]byte(stored), &msgs); err != nil {
		return nil
	}
	return msgs
}

// ReplayRequestMessages reconstructs one call's full request from its chain. It returns the
// messages as a JSON array string plus the system prompt and tool list in effect, matching
// what the row would have stored before delta encoding. A self-contained row costs no extra
// query.
func ReplayRequestMessages(db *sql.DB, callID int64, chain CallChain, stored, system, tools string) (msgsJSON, sysOut, toolsOut string) {
	if db == nil || chain.Key() == "" || !isDeltaEncoded(stored) {
		return stored, system, tools
	}
	where, scope := chain.filter()
	rows, err := db.Query(
		`SELECT COALESCE(request_messages,''), COALESCE(request_system,''), COALESCE(request_tools,'')
		   FROM of_llm_calls WHERE `+where+` AND id <= ? ORDER BY id ASC`, chain.Source, scope, callID)
	if err != nil {
		return stored, system, tools
	}
	defer rows.Close()

	var rp RequestReplayer
	var msgs []json.RawMessage
	sysOut, toolsOut = system, tools
	for rows.Next() {
		var m, s, tl string
		if err := rows.Scan(&m, &s, &tl); err != nil {
			continue
		}
		msgs, sysOut, toolsOut = rp.Next(chain, m, s, tl)
	}
	if err := rows.Err(); err != nil {
		return stored, system, tools
	}
	if msgs == nil {
		return stored, system, tools
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return stored, system, tools
	}
	return string(b), sysOut, toolsOut
}
