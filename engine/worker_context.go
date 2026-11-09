package engine

// Stage support surface and owner/prompt context builders used by
// stage_plan / stage_build / stage_live. These are read helpers with no control
// flow of their own; worker.go holds the worker lifecycle and command/drive
// loops.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"openfabrica/config"
	"openfabrica/db/models"
	"openfabrica/internal/replylang"
	"openfabrica/pack"
	"openfabrica/providers/llm"
)

// defaultPublicPort is the public server port when deps.PublicPort is unset. It
// tracks config.DefaultPublicPort so the fallback and the real default can never
// diverge.
const defaultPublicPort = config.DefaultPublicPort

// newStageSession builds the session for one stage execution, with UI events
// wired.
func (w *Worker) newStageSession(stage string) *session {
	runID := ""
	if w.run != nil {
		runID = w.run.ID
	}
	s := newSession(w.deps, w.pdb, w.projectID, runID, w.limiter, w.dbLog, w.logger, w.sessionEvents(stage))
	// One wiring point for every stage: whatever the owner is watching, the stop
	// button reaches the loop running it, and the worker knows work is in flight.
	s.stopFn = w.stopRequested
	s.busyFn = w.sessionBusy
	if w.providerOverride != nil {
		s.resolver = w.providerOverride
	}
	return s
}

// providerSrc returns this worker's provider source: the test override if set,
// else a fresh resolver for the project. One place decides override-vs-default
// so the session path and the one-shot title path resolve the same way.
func (w *Worker) providerSrc() providerSource {
	if w.providerOverride != nil {
		return w.providerOverride
	}
	return newProviderResolver(w.deps, w.projectID)
}

// projectBrief returns the owner's project description.
func (w *Worker) projectBrief() string {
	project, err := models.GetProjectByID(w.deps.DB.DB, w.projectID)
	if err != nil || project.Description == nil {
		return ""
	}
	return strings.TrimSpace(*project.Description)
}

// peekWakeAnswer returns the owner-answer payload from a resume WITHOUT
// consuming it. Stages running under the drive loop use this: drive() clears
// the payload only after the stage run leaves the transient-retry path
// (clearWakeAnswer), so a stage retried after a transient failure still sees
// the answer it was woken with instead of proceeding answerless.
func (w *Worker) peekWakeAnswer() string {
	if w.wakeContext == nil {
		return ""
	}
	a, _ := w.wakeContext["answer"].(string)
	return strings.TrimSpace(a)
}

// clearWakeAnswer drops the wake payload once its stage run is settled
// (pause, completion, advance, or terminal failure - resumes refold answers
// from the DB). drive() deliberately skips this on a transient stage error so
// the in-place retry re-reads the same answer.
func (w *Worker) clearWakeAnswer() { w.wakeContext = nil }

// takeWakeAnswer returns and clears the owner-answer payload from a resume.
// Only the LIVE command path uses read-and-clear: handleLiveResume runs
// outside the drive loop, so there is no retry that could need a re-read.
func (w *Worker) takeWakeAnswer() string {
	a := w.peekWakeAnswer()
	w.wakeContext = nil
	return a
}

// effectivePacks loads the embedded platform pack set (stage/capability/path/
// fragment). Guides are read-only repo content - there is no DB overlay.
func (w *Worker) effectivePacks() []pack.Pack {
	ps, err := pack.Embedded()
	if err != nil {
		w.logger.Warn("loading platform packs failed", "error", err)
		return nil
	}
	return ps
}

// ownerContext renders the leading owner-context user message body. Lives in
// the message stream (not the system prompt) and is byte-stable for the
// project's lifetime: only the owner identity, NOT the volatile date - which is
// a separate trailing message (dateContextMessage) so a UTC-midnight rollover
// never invalidates the cached prefix that begins at this message.
func (w *Worker) ownerContext() string {
	var lines []string
	var displayName, username string
	err := w.deps.DB.DB.QueryRow(
		"SELECT display_name, username FROM users WHERE role = 'admin' ORDER BY id LIMIT 1",
	).Scan(&displayName, &username)
	owner := strings.TrimSpace(displayName)
	if owner == "" {
		owner = strings.TrimSpace(username)
	}
	if err == nil && owner != "" {
		lines = append(lines, "- Owner: "+owner)
	}
	// Part of the owner context rather than the system prompt: the system prompt is
	// shared across projects and must stay byte-identical, while this block is already
	// per-project. A project with no reply language adds nothing here.
	if line := replylang.Line(w.replyLanguage()); line != "" {
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		// Nothing to say: an empty "## Context" block rode every request otherwise.
		return ""
	}
	return "## Context\n" + strings.Join(lines, "\n") + "\n\n"
}

// replyLanguage returns the project's reply-language directive ("", "match", or a
// language code), as last recorded by a request that woke the engine. It is read from
// the database on every call, never cached on the worker, so a rebuilt seed after a
// context rollover or a restart sees the same value the live worker would.
func (w *Worker) replyLanguage() string {
	if w.deps == nil || w.deps.DB == nil {
		return ""
	}
	p, err := models.GetProjectByID(w.deps.DB.DB, w.projectID)
	if err != nil {
		return ""
	}
	return models.ProjectReplyLanguage(p)
}

// dateContextMessage renders the volatile "today" line as its own trailing,
// UNCACHED user message, kept OUT of the leading owner-context message so a
// UTC-midnight rollover never re-bills the cached prefix: appended after the
// seed's cache breakpoints, it only re-processes the short tail.
func dateContextMessage() llm.Message {
	return llm.Message{
		Role:    llm.RoleUser,
		Content: dateContextSentinel + "\n- Today (UTC): " + time.Now().UTC().Format("2006-01-02") + "\n</date_context>",
	}
}

// ownerMemoryBlock surfaces durable owner preferences (of_memory categories
// the planner/live agent honour).
func (w *Worker) ownerMemoryBlock() string {
	rows, err := w.pdb.Reader().Query(
		`SELECT key, value FROM of_memory WHERE category IN ('preferences','decisions','facts') ORDER BY key`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	omitted := 0
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) != nil {
			continue
		}
		// The block rides the cached prefix of every call. manage_memory allows 100
		// entries of 500 characters, so cap what is surfaced here and say how many
		// were left out rather than let memory grow every prompt.
		if b.Len() >= maxOwnerMemoryBytes {
			omitted++
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", k, v)
	}
	if rows.Err() != nil {
		return ""
	}
	if b.Len() == 0 {
		return ""
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "- (%d more entries not shown; manage_memory list returns them all)\n", omitted)
	}
	return "\nOwner preferences on record:\n" + b.String()
}

// maxOwnerMemoryBytes bounds the owner-preferences block in the prompt prefix.
const maxOwnerMemoryBytes = 8 * 1024

// ownerContextMessage wraps the owner context in its sentinel as the leading
// user message.
func ownerContextMessage(body string) llm.Message {
	return llm.Message{
		Role:    llm.RoleUser,
		Content: ownerContextSentinel + "\n" + body + "</owner_context>",
	}
}

// probeTarget returns the local base URL + vhost Host header for validators.
func (w *Worker) probeTarget() (baseURL, hostHeader string) {
	port := w.deps.PublicPort
	if port == 0 {
		port = defaultPublicPort
	}
	baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	var domain sql.NullString
	_ = w.deps.DB.DB.QueryRow("SELECT domain FROM projects WHERE id = ?", w.projectID).Scan(&domain)
	if domain.Valid && domain.String != "" {
		hostHeader = domain.String
	}
	return baseURL, hostHeader
}

// pendingQuestionCount counts unanswered owner questions.
func (w *Worker) pendingQuestionCount() int {
	return w.countPendingQuestions("")
}

// stampLiveQuestions marks the questions a LIVE or scheduled session just asked
// as live-origin, so their answers re-enter the live agent instead of being
// dismissed as stale build clarifications.
func (w *Worker) stampLiveQuestions() {
	if _, err := w.pdb.ExecWrite(
		"UPDATE of_questions SET origin = ? WHERE status = 'pending' AND origin = ?",
		QuestionOriginLive, QuestionOriginBuild); err != nil {
		w.logger.Warn("stamp live question origin failed", "error", err)
	}
}

// countPendingQuestions counts pending questions, optionally only those of one
// origin (empty = any).
func (w *Worker) countPendingQuestions(origin string) int {
	query, args := "SELECT COUNT(*) FROM of_questions WHERE status = 'pending'", []any{}
	if origin != "" {
		query += " AND origin = ?"
		args = append(args, origin)
	}
	var n int
	if err := w.pdb.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}
