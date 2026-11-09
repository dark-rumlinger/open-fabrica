package ollama

import (
	"net/http"
	"testing"

	"openfabrica/providers/llm"
)

// The matcher used to fire on any 400 whose body contained "think", which meant
// an unrelated complaint could permanently give up the reasoning toggle for a
// model. It now needs unknown-field wording alongside the name.
func TestMatchesThinkRejected(t *testing.T) {
	positives := []string{
		`{"error":"unknown field \"think\""}`,
		`{"error":"think is not supported by this model"}`,
		`{"error":"unrecognized parameter: think"}`,
		`{"error":"invalid option \"think\""}`,
	}
	for _, body := range positives {
		e := &llm.Error{Code: llm.CodeBadRequest, Status: http.StatusBadRequest, Message: body}
		if !matchesThinkRejected(e) {
			t.Errorf("not detected as a think rejection: %s", body)
		}
	}

	negatives := []struct {
		name string
		err  *llm.Error
	}{
		// The daemon complaining about the model's own reasoning output, not the field.
		{"thinking mentioned for another reason", &llm.Error{
			Code: llm.CodeBadRequest, Status: http.StatusBadRequest,
			Message: `{"error":"model produced a think block but streaming is off"}`,
		}},
		{"not a 400", &llm.Error{
			Code: llm.CodeServer, Status: http.StatusInternalServerError,
			Message: `{"error":"unknown field \"think\""}`,
		}},
		{"400 about something else", &llm.Error{
			Code: llm.CodeBadRequest, Status: http.StatusBadRequest,
			Message: `{"error":"unknown field \"frobnicate\""}`,
		}},
	}
	for _, tc := range negatives {
		if matchesThinkRejected(tc.err) {
			t.Errorf("%s: misdetected as a think rejection", tc.name)
		}
	}
}

// Once the field is demoted the native body must omit it, rather than sending
// think=false to a daemon that has no such field. Scoped per model.
func TestNativeThinkField_DemotionIsPerModel(t *testing.T) {
	t.Cleanup(llm.Caps.Reset)
	c := New(llm.Options{Name: "ollama-test"})
	llm.Caps.Learn(c.CapKey("m"), llm.CapNativeThinkField, llm.No)

	if llm.Caps.Allowed(c.CapKey("m"), llm.CapNativeThinkField) {
		t.Error("capability still allowed after being demoted")
	}
	if !llm.Caps.Allowed(c.CapKey("other-model"), llm.CapNativeThinkField) {
		t.Error("a sibling model inherited the demotion")
	}
}
