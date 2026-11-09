package messaging

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/events"
	"openfabrica/tools"
)

// manage_chat is the owner-facing messaging surface: send a non-blocking
// reply, ask a blocking question that pauses the build, or poll for the
// owner's answer. End-user notifications live in manage_notifications.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_chat",
		Category: "messaging",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult:          8000,
		FullResultInEvents: true,
		RequestsPause: func(action string, _ map[string]any) bool {
			return action == "ask"
		},
		OneLiner: "Reply to the project owner, ask a blocking question, or check for answers.",
		WhenToUse: `Use reply for non-blocking updates the owner can read whenever. Use ask only
for genuine blocking ambiguity - it pauses the build until the owner answers.

How ask works: your session ENDS after the call and resumes later with the
owner's answers in your context. Do not poll and do not call check. Ask
everything you need in ONE turn: several ask calls in the same turn reach the
owner as one grouped form. Put a short context line on each saying why it
matters.

Pick the question_type that gives the owner the least typing:
  single_choice  options=["Photo","Illustration"]   pick one (they may still type their own)
  multi_choice   options=["Email","SMS","Push"]     pick several
  text           free-text answer
  secret         secret_name="STRIPE_KEY"            credential, encrypted, never shown to you
  form           fields=[{"name":"key","label":"API key","type":"secret","secret_name":"API_KEY"}]

An answer of "(owner will provide later)" means the owner skipped the question:
choose a sensible default, say so, and do not ask it again.`,
		Actions: []tools.Action{
			&ChatReply{},
			&ChatAsk{},
			&ChatCheck{},
		},
		Hints: func(action string, args map[string]any, r *tools.Result) string {
			if r == nil || !r.Success {
				return ""
			}
			switch action {
			case "reply":
				return "Message sent to the owner."
			case "ask":
				qType, _ := args["question_type"].(string)
				if qType == "secret" {
					return "Secret question: owner's answer will be auto-encrypted and stored via secret_name."
				}
				if fields, _ := args["fields"].([]any); len(fields) > 0 {
					return "Structured form: each field creates a labeled input. Fields with type=\"secret\" auto-encrypt."
				}
				if qType == "multi_choice" {
					return "Multi-select: owner can pick several options; the answer comes back as the chosen labels."
				}
				return "Question queued. This session ends here; you resume with the owner's answer in context. Do not poll or check."
			}
			return ""
		},
	})
}

// ChatReply posts a non-blocking message to the owner via the event bus.
type ChatReply struct {
	Message string `arg:"message,required" desc:"The message to send to the owner."`
}

func (a *ChatReply) Doc() string { return "Send a non-blocking message to the owner." }
func (a *ChatReply) Example() any {
	return &ChatReply{Message: "Pages and tables are in; wiring auth next."}
}
func (a *ChatReply) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	// Persist to the owner chat history (session "brain") so the message
	// survives a reload, mirroring the engine's publishMessage path, then
	// publish it live.
	if ctx.DB != nil {
		if _, err := ctx.DB.Exec(
			"INSERT INTO of_chat_messages (session_id, role, content) VALUES ('brain', 'assistant', ?)",
			a.Message,
		); err != nil && ctx.Logger != nil {
			ctx.Logger.Debug("chat reply persist failed", "error", err)
		}
	}
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventEngineMessage, ctx.ProjectID, map[string]any{
			"session_id": "brain",
			"role":       "assistant",
			"content":    a.Message,
		}))
	}
	return tools.Ok(map[string]any{"sent": true}), nil
}

// maxAskFields bounds a form so the owner never faces a wall of inputs.
const maxAskFields = 12

// ChatAsk poses a blocking question. The session ends after the call; the
// worker parks until the owner answers (admin UI or Telegram), then resumes
// with the answers in the model's context.
type ChatAsk struct {
	Question     string `arg:"question,required" desc:"The question to ask the owner. One clear sentence."`
	Context      string `arg:"context"           desc:"One line on why you are asking and what changes with each answer. Shown under the question."`
	Options      []any  `arg:"options"           desc:"Answer choices as short labels. single_choice: the owner picks one (they can still type their own). multi_choice: any number." items:"string"`
	Urgency      string `arg:"urgency"           desc:"Urgency level. Use high only when the owner is blocking a launch." enum:"low,normal,high" default:"normal"`
	QuestionType string `arg:"question_type"     desc:"single_choice/multi_choice need options. secret needs secret_name. form needs fields. text is free text. Omit it and the type is inferred from options/fields." enum:"text,single_choice,multi_choice,secret,form" default:"text"`
	SecretName   string `arg:"secret_name"       desc:"For question_type='secret': the name to store the secret under."`
	Fields       []any  `arg:"fields"            desc:"For a form: [{\"name\":\"key\",\"label\":\"API key\",\"type\":\"text|secret\",\"secret_name\":\"API_KEY\"}]. Fields with type=secret are encrypted; secret_name defaults to name."`
}

func (a *ChatAsk) Doc() string {
	return "Pose a blocking question. The session ends here and resumes with the owner's answer."
}
func (a *ChatAsk) Example() any {
	return &ChatAsk{
		Question: "Should the homepage hero be photo or illustration?",
		Context:  "Photo needs an image upload from you; illustration I can generate now.",
		Options:  []any{"Photo", "Illustration"},
	}
}

// normalizeAsk infers the question type from what was supplied, so a near-miss
// call (options with the default text type, fields without type=form) succeeds
// instead of erroring. Returns the stored type.
func normalizeAsk(qType, options, fields string) string {
	switch qType {
	case "text", "single_choice", "multi_choice", "secret", "form":
	default:
		qType = "text"
	}
	if fields != "" && qType != "secret" {
		return "form"
	}
	if qType == "text" && options != "" {
		return "single_choice"
	}
	if qType == "form" && fields == "" {
		return "text"
	}
	return qType
}

// normalizeAskFields checks each form field and fills the defaults (type=text,
// secret_name=name for secret fields). Errors say what to re-send.
func normalizeAskFields(raw []any) ([]any, string) {
	if len(raw) > maxAskFields {
		return nil, fmt.Sprintf("a form takes at most %d fields; split it into separate questions", maxAskFields)
	}
	out := make([]any, 0, len(raw))
	seen := map[string]bool{}
	for i, f := range raw {
		m, ok := f.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("fields[%d] must be an object like {\"name\":\"key\",\"label\":\"API key\",\"type\":\"text\"}", i)
		}
		name, _ := m["name"].(string)
		if name == "" {
			return nil, fmt.Sprintf("fields[%d] needs a \"name\"", i)
		}
		if seen[name] {
			return nil, fmt.Sprintf("fields[%d]: duplicate field name %q", i, name)
		}
		seen[name] = true
		if label, _ := m["label"].(string); label == "" {
			m["label"] = name
		}
		switch t, _ := m["type"].(string); t {
		case "", "text":
			m["type"] = "text"
		case "secret":
			if sn, _ := m["secret_name"].(string); sn == "" {
				m["secret_name"] = name
			}
		default:
			return nil, fmt.Sprintf("fields[%d] type %q is not supported; use \"text\" or \"secret\"", i, t)
		}
		out = append(out, m)
	}
	return out, ""
}

func (a *ChatAsk) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if strings.TrimSpace(a.Question) == "" {
		return tools.Fail("question must not be empty"), nil
	}
	if a.Urgency == "" {
		a.Urgency = "normal"
	}
	if len(a.Fields) > 0 {
		normalized, problem := normalizeAskFields(a.Fields)
		if problem != "" {
			return tools.Fail("%s", problem), nil
		}
		a.Fields = normalized
	}
	options := jsonStringArg(a.Options)
	fields := jsonStringArg(a.Fields)
	a.QuestionType = normalizeAsk(a.QuestionType, options, fields)

	if a.QuestionType == "secret" && a.SecretName == "" {
		return tools.Fail("secret_name is required when question_type is 'secret'. Re-call with secret_name set to the name to store it under."), nil
	}
	if a.QuestionType == "secret" && (options != "" || fields != "") {
		return tools.Fail("question_type=\"secret\" cannot be combined with options or fields, secrets always need a password input. Drop them and re-call with just question_type, secret_name, and question."), nil
	}
	if (a.QuestionType == "single_choice" || a.QuestionType == "multi_choice") && options == "" {
		return tools.Fail("question_type=%q requires options for the owner to choose from. Add the options list, or use question_type=\"text\" for a free-text answer.", a.QuestionType), nil
	}

	// The same question asked twice in one run would show the owner two cards
	// for one decision, so hand back the one already waiting.
	var existing int64
	if err := ctx.DB.QueryRow(
		"SELECT id FROM of_questions WHERE status = 'pending' AND question = ? AND COALESCE(options,'') = ? AND COALESCE(fields,'') = ? LIMIT 1",
		a.Question, options, fields,
	).Scan(&existing); err == nil {
		return tools.Ok(map[string]any{
			"id": existing, "question": a.Question, "type": a.QuestionType,
			"note": "This question is already waiting for the owner, so nothing new was queued. Your session ends here and resumes with the answer.",
		}), nil
	}

	result, err := ctx.DB.Exec(
		"INSERT INTO of_questions (question, context, options, urgency, type, secret_name, fields) VALUES (?, ?, ?, ?, ?, ?, ?)",
		a.Question, a.Context, options, a.Urgency, a.QuestionType, a.SecretName, fields,
	)
	if err != nil {
		return nil, fmt.Errorf("creating question: %w", err)
	}
	id, _ := result.LastInsertId()

	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventQuestionAsked, ctx.ProjectID, map[string]any{
			"id": id, "question": a.Question, "context": a.Context,
			"options": options, "urgency": a.Urgency,
			"type": a.QuestionType, "secret_name": a.SecretName, "fields": fields,
		}))
	}
	return tools.Ok(map[string]any{
		"id":       id,
		"question": a.Question,
		"urgency":  a.Urgency,
		"type":     a.QuestionType,
		"note":     "Question queued for the owner (admin dashboard and Telegram). Your session ends here; the build resumes with the answer in context. Do not poll.",
	}), nil
}

// ChatCheck reviews the state of earlier questions. It is not needed to receive
// an answer: the build resumes with answers already in context.
type ChatCheck struct {
	QuestionID int `arg:"question_id" desc:"Specific question id to check (omit to list every pending and answered question)."`
}

func (a *ChatCheck) Doc() string {
	return "Review earlier questions and their answers. Not needed to receive an answer: the build resumes with it."
}
func (a *ChatCheck) ReadOnly() bool { return true }
func (a *ChatCheck) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if a.QuestionID > 0 {
		var question, status string
		var answer sql.NullString
		var answeredAt sql.NullTime
		err := ctx.DB.QueryRow(`
			SELECT q.question, q.status, a.answer, a.created_at
			FROM of_questions q
			LEFT JOIN of_answers a ON a.question_id = q.id
			WHERE q.id = ?
			ORDER BY a.created_at DESC LIMIT 1`,
			a.QuestionID,
		).Scan(&question, &status, &answer, &answeredAt)
		if err == sql.ErrNoRows {
			return tools.NotFound("question id", fmt.Sprint(a.QuestionID)), nil
		}
		if err != nil {
			return nil, fmt.Errorf("checking answer: %w", err)
		}
		data := map[string]any{
			"question_id": a.QuestionID, "question": question,
			"status": status, "answered": answer.Valid,
		}
		if answer.Valid {
			data["answer"] = answer.String
			data["answered_at"] = answeredAt.Time
		}
		return tools.Ok(data), nil
	}

	rows, err := ctx.DB.Query(`
		SELECT q.id, q.question, q.status, a.answer, a.created_at
		FROM of_questions q
		LEFT JOIN of_answers a ON a.question_id = q.id
		WHERE q.status IN ('pending', 'answered', 'deferred')
		ORDER BY q.created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("checking answers: %w", err)
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var qID int
		var question, status string
		var answer sql.NullString
		var answeredAt sql.NullTime
		if err := rows.Scan(&qID, &question, &status, &answer, &answeredAt); err != nil {
			return nil, fmt.Errorf("scanning answer: %w", err)
		}
		entry := map[string]any{
			"question_id": qID, "question": question,
			"status": status, "answered": answer.Valid,
		}
		if answer.Valid {
			entry["answer"] = answer.String
			entry["answered_at"] = answeredAt.Time
		}
		results = append(results, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking answers: %w", err)
	}
	return tools.Ok(results), nil
}
