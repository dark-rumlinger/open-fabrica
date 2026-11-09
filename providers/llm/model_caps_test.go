package llm

import "testing"

func testKey(model string) ModelKey {
	return ModelKey{Provider: "test-provider", Model: model}
}

// Unknown must read as permission, not denial. Anything else would suppress
// every field on every model nothing has spoken about yet, which is most of them.
func TestCapStore_UnknownIsOptimistic(t *testing.T) {
	s := NewCapStore()
	k := testKey("never-seen")
	if got := s.Supports(k, CapTools); got != Unknown {
		t.Fatalf("Supports on an empty store = %v, want Unknown", got)
	}
	if !s.Allowed(k, CapTools) {
		t.Error("Allowed on an unknown capability = false, want true")
	}
}

// A live 400 outranks the catalog. The endpoint is the authority, and an admin
// checkbox left stale must not re-enable a field the wire just rejected.
func TestCapStore_LearnedBeatsDeclared(t *testing.T) {
	s := NewCapStore()
	k := testKey("m")

	s.Declare(k, CapThinkingOff, Yes)
	if !s.Allowed(k, CapThinkingOff) {
		t.Fatal("declared Yes should allow")
	}

	s.Learn(k, CapThinkingOff, No)
	if s.Allowed(k, CapThinkingOff) {
		t.Error("learned No must override declared Yes")
	}

	// A later re-declare must not undo what the wire taught.
	s.Declare(k, CapThinkingOff, Yes)
	if s.Allowed(k, CapThinkingOff) {
		t.Error("re-declaring Yes resurrected a field the endpoint rejected")
	}
}

// Learned() ignores the catalog. Gates that cannot trust a declared No (the DB
// columns are NOT NULL bools filled by name heuristics) read through this.
func TestCapStore_LearnedIgnoresDeclared(t *testing.T) {
	s := NewCapStore()
	k := testKey("m")
	s.Declare(k, CapVision, No)
	if got := s.Learned(k, CapVision); got != Unknown {
		t.Errorf("Learned = %v after only a Declare, want Unknown", got)
	}
	s.Learn(k, CapVision, No)
	if got := s.Learned(k, CapVision); got != No {
		t.Errorf("Learned = %v after a Learn, want No", got)
	}
}

// Learn's return value is the loop bound: the quirk retry keeps going only while
// something new was learned, so a repeat must report false or it never terminates.
func TestCapStore_LearnReportsChangeOnce(t *testing.T) {
	s := NewCapStore()
	k := testKey("m")
	if !s.Learn(k, CapEffort, No) {
		t.Fatal("first Learn = false, want true")
	}
	if s.Learn(k, CapEffort, No) {
		t.Error("repeat Learn = true, want false: the retry loop would not terminate")
	}
}

// The key is (provider, model). Two provider rows can point at the same model
// name over different endpoints, and one row serves many models.
func TestCapStore_KeyIsProviderAndModel(t *testing.T) {
	s := NewCapStore()
	a := ModelKey{Provider: "zai", Model: "glm-5.2"}
	sibling := ModelKey{Provider: "zai", Model: "glm-4.6"}
	proxied := ModelKey{Provider: "zai-via-proxy", Model: "glm-5.2"}

	s.Learn(a, CapThinkingOff, No)

	if !s.Allowed(sibling, CapThinkingOff) {
		t.Error("a sibling model on the same provider inherited the latch")
	}
	if !s.Allowed(proxied, CapThinkingOff) {
		t.Error("the same model on a different provider row inherited the latch")
	}
	if s.Allowed(a, CapThinkingOff) {
		t.Error("the latched pair itself lost its learned value")
	}
}

func TestCapStore_Reset(t *testing.T) {
	s := NewCapStore()
	k := testKey("m")
	s.Learn(k, CapTools, No)
	s.Reset()
	if !s.Allowed(k, CapTools) {
		t.Error("Reset left learned state behind: tests would bleed into each other")
	}
}

// A stripped request must still say something about the missing attachment, or
// the model reasons about an image it was never shown.
func TestStripImages(t *testing.T) {
	req := CompletionRequest{Messages: []Message{
		{Role: RoleUser, Content: "what is this?", Images: []MessageImage{{MIMEType: "image/png", Data: []byte{1}}}},
		{Role: RoleUser, Content: "no image here"},
	}}
	out, dropped := StripImages(req)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(out.Messages[0].Images) != 0 {
		t.Error("image survived the strip")
	}
	if out.Messages[0].Content == "what is this?" {
		t.Error("no marker added: the model is left reasoning about an attachment it cannot see")
	}
	if out.Messages[1].Content != "no image here" {
		t.Error("an image-free message was modified")
	}
	// The caller's slice must not be mutated underneath it.
	if len(req.Messages[0].Images) != 1 {
		t.Error("StripImages mutated the caller's request")
	}
}
