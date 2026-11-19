package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"openfabrica/providers/llm"
)

func TestVisionQuirkLearnsAndRetriesWithoutImages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"1210","message":"messages.content.type is invalid, allowed values: ['text']"}}`))
			return
		}
		if len(body.Messages) == 0 {
			t.Fatal("expected messages on retry")
		}
		if _, isStr := body.Messages[0].Content.(string); !isStr {
			t.Fatalf("expected plain string content after StripImages, got %T", body.Messages[0].Content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(llm.Options{Name: "zai-test", BaseURL: srv.URL, APIKey: "k", HTTPClient: srv.Client()})
	resp, err := c.Complete(context.Background(), llm.CompletionRequest{
		Model: "glm-5.3-test",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "build this",
			Images:  []llm.MessageImage{{MIMEType: "image/png", Data: []byte{1, 2, 3}}},
		}},
	})
	if err != nil {
		t.Fatalf("expected retry to succeed after stripping images, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("got content %q, want %q", resp.Content, "ok")
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls (400 then stripped retry), got %d", calls.Load())
	}
	if got := llm.Caps.Learned(c.CapKey("glm-5.3-test"), llm.CapVision); got != llm.No {
		t.Errorf("expected CapVision learned No, got %v", got)
	}
}
