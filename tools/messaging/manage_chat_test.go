package messaging

import (
	"strings"
	"testing"

	"openfabrica/events"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// The reply tool's non-blocking message must reach the live chat via the
// engine.message event the admin UI subscribes to.
func TestReply_PublishesEngineMessageEvent(t *testing.T) {
	bus := events.NewBus()
	var got events.Event
	bus.Subscribe(events.EventEngineMessage, func(e events.Event) { got = e })
	ctx := &tools.ToolContext{Bus: bus, ProjectID: 7}

	res, err := (&ChatReply{Message: "Pages and tables are in."}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("reply must succeed: %+v", res)
	}
	// Publish runs handlers inline, so got is populated by the time Run returns.
	if got.Type != events.EventEngineMessage {
		t.Fatalf("reply must publish %q, got %q", events.EventEngineMessage, got.Type)
	}
	if got.Payload["content"] != "Pages and tables are in." {
		t.Fatalf("reply payload content = %v, want the message text", got.Payload["content"])
	}
}

func askCtx(t *testing.T) (*tools.ToolContext, *events.Bus, *[]events.Event) {
	t.Helper()
	bus := events.NewBus()
	var asked []events.Event
	bus.Subscribe(events.EventQuestionAsked, func(e events.Event) { asked = append(asked, e) })
	return &tools.ToolContext{DB: tooltest.ProjectDB(t), Bus: bus, ProjectID: 3}, bus, &asked
}

func storedType(t *testing.T, ctx *tools.ToolContext, id int64) string {
	t.Helper()
	var typ string
	if err := ctx.DB.QueryRow("SELECT type FROM of_questions WHERE id = ?", id).Scan(&typ); err != nil {
		t.Fatalf("load question %d: %v", id, err)
	}
	return typ
}

func askID(t *testing.T, res *tools.Result) int64 {
	t.Helper()
	m, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result data %T", res.Data)
	}
	id, _ := m["id"].(int64)
	return id
}

func TestAsk_InsertsPendingRowAndPublishes(t *testing.T) {
	ctx, _, asked := askCtx(t)
	res, err := (&ChatAsk{Question: "Photo or illustration?", Context: "affects hero", Options: []any{"Photo", "Illustration"}}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("ask failed: %v %+v", err, res)
	}
	var status, qType, urgency string
	if err := ctx.DB.QueryRow("SELECT status, type, urgency FROM of_questions WHERE id = ?", askID(t, res)).Scan(&status, &qType, &urgency); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || urgency != "normal" {
		t.Fatalf("status=%q urgency=%q, want pending/normal", status, urgency)
	}
	if qType != "single_choice" {
		t.Fatalf("options with default type must be inferred as single_choice, got %q", qType)
	}
	if len(*asked) != 1 || (*asked)[0].Payload["context"] != "affects hero" {
		t.Fatalf("expected one question.asked event carrying the context, got %+v", *asked)
	}
}

func TestAsk_InfersFormFromFields(t *testing.T) {
	ctx, _, _ := askCtx(t)
	res, _ := (&ChatAsk{
		Question: "Stripe keys?",
		Fields: []any{
			map[string]any{"name": "pk", "label": "Publishable key"},
			map[string]any{"name": "sk", "type": "secret"},
		},
	}).Run(ctx)
	if !res.Success {
		t.Fatalf("form ask failed: %+v", res)
	}
	id := askID(t, res)
	if got := storedType(t, ctx, id); got != "form" {
		t.Fatalf("fields must be stored as type=form, got %q", got)
	}
	var fields string
	_ = ctx.DB.QueryRow("SELECT fields FROM of_questions WHERE id = ?", id).Scan(&fields)
	if !strings.Contains(fields, `"secret_name":"sk"`) || !strings.Contains(fields, `"label":"sk"`) {
		t.Fatalf("secret field must default secret_name and label to its name: %s", fields)
	}
}

func TestAsk_RejectsBadInput(t *testing.T) {
	ctx, _, _ := askCtx(t)
	cases := map[string]*ChatAsk{
		"empty question":       {Question: "  "},
		"secret without name":  {Question: "Key?", QuestionType: "secret"},
		"secret with options":  {Question: "Key?", QuestionType: "secret", SecretName: "K", Options: []any{"a"}},
		"choice without opts":  {Question: "Pick", QuestionType: "single_choice"},
		"field without name":   {Question: "F", Fields: []any{map[string]any{"label": "x"}}},
		"field bad type":       {Question: "F", Fields: []any{map[string]any{"name": "a", "type": "date"}}},
		"duplicate field name": {Question: "F", Fields: []any{map[string]any{"name": "a"}, map[string]any{"name": "a"}}},
		"field not an object":  {Question: "F", Fields: []any{"a"}},
	}
	for name, a := range cases {
		if res, _ := a.Run(ctx); res == nil || res.Success {
			t.Errorf("%s: expected a failure result, got %+v", name, res)
		}
	}
	var n int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_questions").Scan(&n)
	if n != 0 {
		t.Fatalf("rejected asks must not insert rows, found %d", n)
	}
}

func TestAsk_DedupesIdenticalPendingQuestion(t *testing.T) {
	ctx, _, asked := askCtx(t)
	first, _ := (&ChatAsk{Question: "Domain name?"}).Run(ctx)
	second, _ := (&ChatAsk{Question: "Domain name?"}).Run(ctx)
	if askID(t, first) != askID(t, second) {
		t.Fatalf("repeat ask must return the pending id: %d vs %d", askID(t, first), askID(t, second))
	}
	if len(*asked) != 1 {
		t.Fatalf("repeat ask must not publish again, got %d events", len(*asked))
	}
}

func TestManageChat_OnlyAskRequestsPause(t *testing.T) {
	def, err := tools.DefaultRegistry().Get("manage_chat")
	if err != nil {
		t.Fatalf("manage_chat not registered: %v", err)
	}
	for action, want := range map[string]bool{"ask": true, "reply": false, "check": false} {
		if got := def.RequestsPause(action, nil); got != want {
			t.Errorf("RequestsPause(%q) = %v, want %v", action, got, want)
		}
	}
}
