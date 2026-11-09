package bot

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"openfabrica/engine"
	"openfabrica/events"
	"openfabrica/internal/safego"
)

// --- Question answering ---

// DeferredAnswerText is the sentinel stored in of_answers when the owner opts out
// of a question. The engine owns the text and explains it to the planner, so the
// model does not re-ask. The admin uses the same text.
const DeferredAnswerText = engine.DeferredAnswerText

// deferQuestion marks a pending question "answer later", exactly as the admin's
// Later button does: status deferred, sentinel answer, and an answered event so
// the engine resumes. Idempotent. Returns the number of still-pending questions.
func (s *BotService) deferQuestion(state *UserState, questionID string) (int, bool) {
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return -1, false
	}
	var answeredBy any
	if id, err := strconv.ParseInt(state.UserID, 10, 64); err == nil {
		answeredBy = id
	}
	var affected int64
	err := pdb.WithTx(func(tx *sql.Tx) error {
		res, e := tx.Exec("UPDATE of_questions SET status='deferred', updated_at=datetime('now') WHERE id = ? AND status='pending'", questionID)
		if e != nil {
			return e
		}
		affected, _ = res.RowsAffected()
		if affected == 0 {
			return nil
		}
		_, e = tx.Exec("INSERT INTO of_answers (question_id, answer, answered_by) VALUES (?, ?, ?)", questionID, DeferredAnswerText, answeredBy)
		return e
	})
	if err != nil {
		s.logger.Warn("deferQuestion: tx failed", "error", err, "question_id", questionID)
		return -1, false
	}
	if affected > 0 {
		s.deps.Bus.Publish(events.NewEvent(events.EventQuestionAnswered, state.ActiveProjectID, map[string]any{
			"question_id": questionID,
			"answer":      DeferredAnswerText,
			"deferred":    true,
		}))
	}
	return s.pendingQuestionCount(pdb), true
}

// handleQuestionNav routes q:<open|defer|deferall> callbacks. Caller holds state.Mu.
func (s *BotService) handleQuestionNav(state *UserState, messageID int, rest string) string {
	v := s.voice(state)
	if state.ActiveProjectID == 0 {
		s.showScreen(state, messageID, s.mainMenuScreen(v))
		return v.tr("Pick a project first")
	}
	parts := strings.SplitN(rest, ":", 2)
	switch parts[0] {
	case "open":
		if len(parts) == 2 {
			s.promptQuestionByID(state, parts[1])
		}
	case "defer":
		if len(parts) != 2 {
			return ""
		}
		remaining, ok := s.deferQuestion(state, parts[1])
		if !ok {
			return v.tr("Couldn't skip that")
		}
		s.afterDefer(state, remaining)
		return v.tr("I'll ask again later")
	case "deferall":
		pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
		if pdb == nil {
			return ""
		}
		rows, err := pdb.Query("SELECT id FROM of_questions WHERE status='pending' ORDER BY id")
		if err != nil {
			return ""
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		iterErr := rows.Err()
		rows.Close()
		if iterErr != nil {
			return v.tr("Could not read the questions")
		}
		for _, id := range ids {
			s.deferQuestion(state, id)
		}
		s.clearPending(state)
		s.showScreen(state, messageID, s.questionsScreen(state))
		return v.tr("Skipped %d", len(ids))
	}
	return ""
}

// afterDefer follows a skipped question with the next one, or lets the builder resume.
func (s *BotService) afterDefer(state *UserState, remaining int) {
	v := s.voice(state)
	if remaining <= 0 {
		s.clearPending(state)
		s.send(state, v.tr("Okay, I'll ask again later. Open Fabrica is resuming..."))
		return
	}
	s.send(state, v.tr("Okay, later. %d question(s) remaining.", remaining))
	s.promptNextPending(state)
}

// recordAnswer atomically records an answer to a pending question. It is
// idempotent (a double-tap or redelivered update is a no-op), attributed
// (answered_by), and secret-aware (a type='secret' answer is encrypted into
// of_secrets and the user's pasted message is deleted). Returns the number of
// still-pending questions, and ok=false if nothing was recorded.
func (s *BotService) recordAnswer(state *UserState, questionIDStr, answerText string, userMsgID int) (int, bool) {
	if state.ActiveProjectID == 0 {
		return -1, false
	}
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return -1, false
	}
	v := s.voice(state)

	var qType, secretName string
	if err := pdb.QueryRow("SELECT COALESCE(type,'text'), COALESCE(secret_name,'') FROM of_questions WHERE id = ?", questionIDStr).
		Scan(&qType, &secretName); err != nil {
		s.logger.Warn("recordAnswer: question lookup failed", "error", err, "question_id", questionIDStr)
		return -1, false
	}

	storedAnswer := answerText
	encryptedSecret := ""
	if qType == "secret" {
		if secretName == "" {
			s.send(state, v.tr("This secret question is misconfigured (no name). Please answer it in the admin UI."))
			return -1, false
		}
		if s.deps.Encryptor == nil {
			s.send(state, v.tr("Secrets can't be stored from chat right now. Please use the admin UI."))
			return -1, false
		}
		enc, err := s.deps.Encryptor.Encrypt(answerText)
		if err != nil {
			s.logger.Warn("recordAnswer: encrypt failed", "error", err)
			s.send(state, v.tr("Failed to store the secret securely. Please try again."))
			return -1, false
		}
		encryptedSecret = enc
		storedAnswer = "(secret provided)"
	}

	var answeredBy any
	if state.UserID != "" {
		if id, err := strconv.ParseInt(state.UserID, 10, 64); err == nil {
			answeredBy = id
		}
	}

	var affected int64
	txErr := pdb.WithTx(func(tx *sql.Tx) error {
		// Status guard makes the write idempotent: a second tap affects 0 rows.
		res, e := tx.Exec("UPDATE of_questions SET status='answered', updated_at=datetime('now') WHERE id = ? AND status='pending'", questionIDStr)
		if e != nil {
			return e
		}
		affected, _ = res.RowsAffected()
		if affected == 0 {
			return nil
		}
		if qType == "secret" {
			if _, e := tx.Exec(
				`INSERT INTO of_secrets (name, value_encrypted, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
				 ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
				secretName, encryptedSecret); e != nil {
				return e
			}
		}
		if _, e := tx.Exec("INSERT INTO of_answers (question_id, answer, answered_by) VALUES (?, ?, ?)",
			questionIDStr, storedAnswer, answeredBy); e != nil {
			return e
		}
		return nil
	})
	if txErr != nil {
		s.logger.Warn("recordAnswer: tx failed", "error", txErr, "question_id", questionIDStr)
		s.send(state, v.tr("Failed to record your answer. Please try again."))
		return -1, false
	}

	// Scrub the pasted secret from the chat once it's stored (or already was).
	if qType == "secret" && userMsgID > 0 {
		if p, ok := s.provider(state.Provider); ok {
			_ = p.DeleteMessage(state.ChatID, userMsgID)
		}
	}

	if affected == 0 {
		return s.pendingQuestionCount(pdb), true // already answered: no event
	}

	s.deps.Bus.Publish(events.NewEvent(events.EventQuestionAnswered, state.ActiveProjectID, map[string]any{
		"question_id": questionIDStr,
		"answer":      storedAnswer,
	}))
	return s.pendingQuestionCount(pdb), true
}

// answerPendingQuestion records a free-text reply against the question the user
// was shown (preferredID), falling back to the oldest pending one when that is
// gone or already answered, then drives the follow-up. If nothing is pending the
// reply is just ordinary chat.
func (s *BotService) answerPendingQuestion(state *UserState, preferredID, text string, msgID int) {
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		s.handleChat(state, text)
		return
	}
	var qid string
	if preferredID != "" {
		var status string
		if err := pdb.QueryRow("SELECT status FROM of_questions WHERE id = ?", preferredID).Scan(&status); err == nil && status == "pending" {
			qid = preferredID
		}
	}
	if qid == "" {
		if err := pdb.QueryRow("SELECT id FROM of_questions WHERE status='pending' ORDER BY id LIMIT 1").Scan(&qid); err != nil {
			s.handleChat(state, text) // stale awaiting flow; treat as chat
			return
		}
	}
	remaining, ok := s.recordAnswer(state, qid, text, msgID)
	if !ok {
		s.promptNextPending(state) // re-arm so the user can retry
		return
	}
	s.afterAnswer(state, remaining)
}

// answerQuestionByIndex resolves the chosen option index against the question's
// stored options array, then records the full answer text via recordAnswer.
func (s *BotService) answerQuestionByIndex(state *UserState, questionIDStr, indexStr string) {
	if state.ActiveProjectID == 0 {
		return
	}
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return
	}

	var optionsJSON string
	if err := pdb.QueryRow("SELECT COALESCE(options,'') FROM of_questions WHERE id = ?", questionIDStr).Scan(&optionsJSON); err != nil {
		s.logger.Warn("answerQuestionByIndex: question lookup failed", "error", err, "question_id", questionIDStr)
		return
	}

	idx := -1
	if _, err := fmt.Sscanf(indexStr, "%d", &idx); err != nil || idx < 0 {
		return
	}
	opts := parseOptionsJSON(optionsJSON)
	if idx >= len(opts) || opts[idx] == "" {
		return
	}

	remaining, ok := s.recordAnswer(state, questionIDStr, opts[idx], 0)
	if !ok {
		return
	}
	s.afterAnswer(state, remaining)
}

// afterAnswer reports progress and, if more questions remain, re-prompts the
// next one (keeping the answer flow armed). When none remain it clears the flow.
func (s *BotService) afterAnswer(state *UserState, remaining int) {
	v := s.voice(state)
	if remaining <= 0 {
		state.PendingAction = ""
		state.PendingData = nil
		state.PendingActionAt = time.Time{}
		SaveSession(s.deps.DB, state)
		s.send(state, v.tr("Answer received. Open Fabrica is resuming..."))
		return
	}
	s.send(state, v.tr("Answer received. %d question(s) remaining.", remaining))
	s.promptNextPending(state)
}

// deliverNextIfIdle delivers the oldest pending question, unless the user is
// already actively answering one that is still pending. This keeps a batch of
// questions from being dumped all at once: they surface one at a time, so a
// free-text reply is unambiguous about which question it answers. Caller must
// hold state.Mu.
func (s *BotService) deliverNextIfIdle(state *UserState) {
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return
	}
	// Already showing a still-pending question? Don't stack another on top.
	if state.PendingAction == "awaiting_answer" && state.PendingData != nil {
		if qid := state.PendingData["question_id"]; qid != "" {
			var status string
			if err := pdb.QueryRow("SELECT status FROM of_questions WHERE id = ?", qid).Scan(&status); err == nil && status == "pending" {
				return
			}
		}
	}
	s.promptNextPending(state)
}

// promptNextPending delivers the oldest still-pending question (if any).
func (s *BotService) promptNextPending(state *UserState) {
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return
	}
	var qid string
	if err := pdb.QueryRow("SELECT id FROM of_questions WHERE status='pending' ORDER BY id LIMIT 1").Scan(&qid); err != nil {
		return
	}
	s.promptQuestionByID(state, qid)
}

// scheduleResend re-sends still-pending questions shortly after the first
// provider starts, once per process. A restart loses nothing in the database,
// but the owner may never have seen the question, and nothing else re-sends it.
func (s *BotService) scheduleResend() {
	s.resendOnce.Do(func() {
		safego.Go("bot-resend-questions", func() {
			time.Sleep(3 * time.Second)
			s.resendPendingQuestions()
		})
	})
}

// resendPendingQuestions tells each known user how many questions still wait and
// shows the oldest one. Users in the middle of a form are left alone.
func (s *BotService) resendPendingQuestions() {
	s.mu.RLock()
	users := make([]*UserState, 0, len(s.users))
	for _, state := range s.users {
		users = append(users, state)
	}
	s.mu.RUnlock()

	for _, state := range users {
		state.Mu.Lock()
		s.resendFor(state)
		state.Mu.Unlock()
	}
}

// resendFor is resendPendingQuestions for one user. Caller holds state.Mu.
func (s *BotService) resendFor(state *UserState) {
	if state.ActiveProjectID == 0 || state.PendingAction == "awaiting_form" {
		return
	}
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return
	}
	n := s.pendingQuestionCount(pdb)
	if n == 0 {
		return
	}
	s.send(state, s.voice(state).tr("I'm back. %d question(s) still waiting for you.", n))
	s.promptNextPending(state)
}

// questionMeta is the owner-facing framing around a question: why it is asked,
// how urgent it is, and where it sits in a batch.
type questionMeta struct {
	context  string
	urgency  string
	position int
	total    int
}

// promptQuestionByID loads a question from the project DB and delivers it,
// arming the answer flow. Caller must hold state.Mu.
func (s *BotService) promptQuestionByID(state *UserState, questionID string) {
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return
	}
	var question, optionsJSON, qType, secretName, fieldsJSON, status string
	var meta questionMeta
	err := pdb.QueryRow(
		`SELECT question, COALESCE(options,''), COALESCE(type,'text'), COALESCE(secret_name,''), COALESCE(fields,''), COALESCE(status,'pending'),
		        COALESCE(context,''), COALESCE(urgency,'normal')
		 FROM of_questions WHERE id = ?`, questionID).
		Scan(&question, &optionsJSON, &qType, &secretName, &fieldsJSON, &status, &meta.context, &meta.urgency)
	if err != nil {
		s.logger.Warn("promptQuestionByID: lookup failed", "error", err, "question_id", questionID)
		return
	}
	if status != "pending" {
		return // already answered elsewhere (e.g. the web UI)
	}
	meta.total = s.pendingQuestionCount(pdb)
	_ = pdb.QueryRow("SELECT COUNT(*) FROM of_questions WHERE status='pending' AND id <= ?", questionID).Scan(&meta.position)
	// Approval bookkeeping lives in the context column; it is not for the owner.
	if strings.HasPrefix(meta.context, "approval:") {
		meta.context = ""
	}
	s.deliverQuestionMeta(state, questionID, question, parseOptionsJSON(optionsJSON), qType, secretName, fieldsJSON, meta)
}

// deliverQuestion sends a question with no batch framing. See deliverQuestionMeta.
func (s *BotService) deliverQuestion(state *UserState, questionID, question string, options []string, qType, secretName, fieldsJSON string) {
	s.deliverQuestionMeta(state, questionID, question, options, qType, secretName, fieldsJSON, questionMeta{})
}

// questionBody renders the question text with its framing (position in the batch,
// urgency, context). The question and context are model text, so they are escaped
// for the HTML parse mode.
func questionBody(v voice, question string, meta questionMeta) string {
	var b strings.Builder
	if meta.total > 1 && meta.position > 0 {
		b.WriteString(v.trh("<b>Question %d of %d</b>", meta.position, meta.total) + "\n")
	}
	if meta.urgency == "high" {
		b.WriteString("🔴 ")
	}
	b.WriteString(htmlf("%s", question))
	if meta.context != "" {
		b.WriteString(htmlf("\n\n<i>%s</i>", meta.context))
	}
	return b.String()
}

// selectedSet parses the comma-separated option indexes stored while a
// multi-choice question is being answered.
func selectedSet(raw string) map[int]bool {
	set := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			set[n] = true
		}
	}
	return set
}

// choiceScreen builds the buttons for an option question. A multi-choice question
// gets toggle buttons (m:<id>:<index>) with a Done button; every other option
// question gets one-tap answer buttons (a:<id>:<index>). Callbacks carry the index,
// not the label, to stay under Telegram's 64-byte callback_data cap.
func choiceScreen(v voice, questionID, body, qType string, options []string, selected map[int]bool) screen {
	labels := make([]string, 0, len(options)+2)
	cbs := make([]string, 0, len(options)+2)
	multi := qType == "multi_choice"
	for i, opt := range options {
		if multi {
			if selected[i] {
				opt = "✅ " + opt
			}
			cbs = append(cbs, fmt.Sprintf("m:%s:%d", questionID, i))
		} else {
			cbs = append(cbs, fmt.Sprintf("a:%s:%d", questionID, i))
		}
		labels = append(labels, opt)
	}
	switch {
	case multi:
		body += "\n\n" + v.tr("Tap every option that applies, then press Done. You can also type your own answer.")
		labels = append(labels, v.tr("✔ Done"))
		cbs = append(cbs, fmt.Sprintf("m:%s:done", questionID))
	case qType != "approval":
		body += "\n\n" + v.tr("Tap an option, or type your own answer.")
	}
	labels = append(labels, v.tr("⏭ Later"))
	cbs = append(cbs, fmt.Sprintf("q:defer:%s", questionID))
	return screen{text: body, labels: labels, cbs: cbs}
}

// toggleMultiChoice flips one option of a multi-choice question (or submits the
// selection on "done") and redraws the buttons in place. Caller holds state.Mu.
func (s *BotService) toggleMultiChoice(state *UserState, messageID int, questionID, arg string) string {
	if state.ActiveProjectID == 0 {
		return ""
	}
	v := s.voice(state)
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return ""
	}
	var question, optionsJSON, status string
	meta := questionMeta{}
	if err := pdb.QueryRow(
		`SELECT question, COALESCE(options,''), COALESCE(status,'pending'), COALESCE(context,''), COALESCE(urgency,'normal')
		 FROM of_questions WHERE id = ?`, questionID).
		Scan(&question, &optionsJSON, &status, &meta.context, &meta.urgency); err != nil {
		return ""
	}
	if status != "pending" {
		return v.tr("Already answered")
	}
	options := parseOptionsJSON(optionsJSON)

	// Arm the free-text path for this question and drop a selection that belongs to
	// a different one.
	if state.PendingData == nil || state.PendingData["question_id"] != questionID {
		state.PendingData = map[string]string{"question_id": questionID}
	}
	state.PendingAction = "awaiting_answer"
	state.PendingActionAt = time.Now()
	selected := selectedSet(state.PendingData["selected"])

	if arg == "done" {
		var picked []string
		for i, opt := range options {
			if selected[i] {
				picked = append(picked, opt)
			}
		}
		if len(picked) == 0 {
			return v.tr("Pick at least one option")
		}
		remaining, ok := s.recordAnswer(state, questionID, strings.Join(picked, ", "), 0)
		if !ok {
			return v.tr("Couldn't record that")
		}
		s.afterAnswer(state, remaining)
		return v.tr("Answer recorded")
	}

	idx, err := strconv.Atoi(arg)
	if err != nil || idx < 0 || idx >= len(options) {
		return ""
	}
	if selected[idx] {
		delete(selected, idx)
	} else {
		selected[idx] = true
	}
	var parts []string
	for i := range options {
		if selected[i] {
			parts = append(parts, strconv.Itoa(i))
		}
	}
	state.PendingData["selected"] = strings.Join(parts, ",")
	SaveSession(s.deps.DB, state)

	s.showScreen(state, messageID, choiceScreen(v, questionID, questionBody(v, question, meta), "multi_choice", options, selected))
	return ""
}

// deliverQuestionMeta sends a question to the user and arms the answer flow. Form
// questions (with field definitions) start a sequential field-by-field flow;
// secret questions ask for a value to store; option questions get inline
// buttons; everything else is free text. Caller must hold state.Mu.
func (s *BotService) deliverQuestionMeta(state *UserState, questionID, question string, options []string, qType, secretName, fieldsJSON string, meta questionMeta) {
	p, ok := s.provider(state.Provider)
	if !ok {
		return
	}
	v := s.voice(state)

	// A question with field definitions is a multi-field form (mirrors the admin:
	// fields take precedence over type, except an approval is never a form).
	if qType != "approval" && len(parseFormFields(fieldsJSON)) > 0 {
		s.startFormFlow(state, questionID, question, fieldsJSON)
		return
	}

	state.PendingAction = "awaiting_answer"
	state.PendingActionAt = time.Now()
	state.PendingData = map[string]string{"question_id": questionID}
	SaveSession(s.deps.DB, state)

	body := questionBody(v, question, meta)
	later := fmt.Sprintf("q:defer:%s", questionID)
	switch {
	case qType == "secret":
		text := "🔐 " + body + "\n\n" + v.tr("Reply with the secret value, I'll store it encrypted and delete your message.")
		if err := p.SendQuestion(state.ChatID, text, []string{v.tr("⏭ Later")}, []string{later}); err != nil {
			if _, e2 := p.SendMessage(state.ChatID, text); e2 != nil {
				s.logger.Error("question delivery failed", "error", e2, "project_id", state.ActiveProjectID)
			}
		}

	case len(options) > 0:
		sc := choiceScreen(v, questionID, body, qType, options, nil)
		if err := p.SendQuestion(state.ChatID, sc.text, sc.labels, sc.cbs); err != nil {
			s.logger.Warn("send question keyboard failed; falling back to text", "error", err)
			if _, e2 := p.SendMessage(state.ChatID, body+"\n\n"+v.tr("Reply with your answer (buttons unavailable):")); e2 != nil {
				s.logger.Error("question delivery failed entirely", "error", e2, "project_id", state.ActiveProjectID)
			}
		}

	default:
		text := body + "\n\n" + v.tr("Reply with your answer:")
		if err := p.SendQuestion(state.ChatID, text, []string{v.tr("⏭ Later")}, []string{later}); err != nil {
			if _, e2 := p.SendMessage(state.ChatID, text); e2 != nil {
				s.logger.Error("question delivery failed", "error", e2, "project_id", state.ActiveProjectID)
			}
		}
	}
}
