package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/providers/imagegen"
)

// Generate must POST to /v1/images/generations with a Bearer key and an
// OpenAI-shaped body (model, prompt, clamped n, mapped quality), then decode
// the base64 image into raw bytes with the revised prompt preserved.
func TestGenerate_ShapesRequestAndDecodesImage(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	rawPNG := []byte("\x89PNG-fake-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		b64 := base64.StdEncoding.EncodeToString(rawPNG)
		io.WriteString(w, `{"data":[{"b64_json":"`+b64+`","revised_prompt":"a red bike, cinematic"}]}`)
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "openai-image", APIKey: "img-key", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	if c.Type() != "openai-image" {
		t.Errorf("Type() = %q, want openai-image", c.Type())
	}
	resp, err := c.Generate(context.Background(), imagegen.GenerateRequest{
		Model:   "gpt-image-1",
		Prompt:  "a red bike",
		N:       9, // must clamp to 4
		Size:    "1024x1024",
		Quality: "high",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if gotPath != "/v1/images/generations" {
		t.Errorf("path = %q, want /v1/images/generations", gotPath)
	}
	if gotAuth != "Bearer img-key" {
		t.Errorf("Authorization = %q, want Bearer img-key", gotAuth)
	}
	if gotBody["model"] != "gpt-image-1" || gotBody["prompt"] != "a red bike" {
		t.Errorf("body model/prompt wrong: %#v", gotBody)
	}
	if n, _ := gotBody["n"].(float64); n != 4 {
		t.Errorf("n = %v, want clamped to 4", gotBody["n"])
	}
	if gotBody["quality"] != "high" {
		t.Errorf("quality = %v, want high", gotBody["quality"])
	}
	if len(resp.Images) != 1 {
		t.Fatalf("want 1 image, got %d", len(resp.Images))
	}
	img := resp.Images[0]
	if string(img.Bytes) != string(rawPNG) {
		t.Errorf("decoded bytes mismatch: %q", img.Bytes)
	}
	if img.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", img.ContentType)
	}
	if img.Revised != "a red bike, cinematic" {
		t.Errorf("Revised = %q", img.Revised)
	}
}

// The adapter is a strict API boundary: a blank prompt or missing model/key is
// rejected before any network call.
func TestGenerate_ValidationErrors(t *testing.T) {
	// No API key.
	c := New(imagegen.Options{Name: "openai-image"})
	if _, err := c.Generate(context.Background(), imagegen.GenerateRequest{Model: "gpt-image-1", Prompt: "x"}); err == nil {
		t.Error("want error when no API key configured")
	}
	// Blank prompt.
	c = New(imagegen.Options{Name: "openai-image", APIKey: "k"})
	if _, err := c.Generate(context.Background(), imagegen.GenerateRequest{Model: "gpt-image-1", Prompt: "   "}); err == nil {
		t.Error("want error on blank prompt")
	}
	// Missing model.
	if _, err := c.Generate(context.Background(), imagegen.GenerateRequest{Prompt: "x"}); err == nil {
		t.Error("want error when model is not resolved")
	}
}

// A non-200 from the API is surfaced with the provider-side error message.
func TestGenerate_APIErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"content policy violation","type":"invalid_request_error","code":"moderation_blocked"}}`)
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "openai-image", APIKey: "k", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	_, err := c.Generate(context.Background(), imagegen.GenerateRequest{Model: "gpt-image-1", Prompt: "x"})
	if err == nil {
		t.Fatal("want an error on 400")
	}
	if !strings.Contains(err.Error(), "content policy violation") {
		t.Errorf("error should carry the provider message, got %q", err.Error())
	}
}

// A response that returns a URL instead of inline base64 (contract break) is
// surfaced clearly rather than dropped.
func TestGenerate_MissingBase64IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"url":"https://cdn.example/img.png"}]}`)
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "openai-image", APIKey: "k", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	_, err := c.Generate(context.Background(), imagegen.GenerateRequest{Model: "gpt-image-1", Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "b64_json") {
		t.Errorf("want a missing-b64_json error, got %v", err)
	}
}

// ListModels filters GET <base>/v1/models to the image families.
func TestListModels_FiltersImageFamilies(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"data":[
			{"id":"gpt-image-1"},
			{"id":"dall-e-3"},
			{"id":"gpt-4o"},
			{"id":"whisper-1"}
		]}`)
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "openai-image", APIKey: "k", BaseURL: srv.URL + "/v1", HTTPClient: srv.Client()})
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("listing path = %q, want /v1/models", gotPath)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if !ids["gpt-image-1"] || !ids["dall-e-3"] {
		t.Errorf("image families should be kept, got %v", ids)
	}
	if ids["gpt-4o"] || ids["whisper-1"] {
		t.Errorf("non-image models should be filtered, got %v", ids)
	}
}

func TestMapQuality(t *testing.T) {
	cases := map[string]string{
		"low": "low", "high": "high", "medium": "medium",
		"": "medium", "standard": "medium", "weird": "medium",
	}
	for in, want := range cases {
		if got := mapQuality(in); got != want {
			t.Errorf("mapQuality(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapabilities(t *testing.T) {
	c := New(imagegen.Options{Name: "openai-image", APIKey: "k"})
	caps := c.Capabilities()
	if caps.MaxN != 4 {
		t.Errorf("MaxN = %d, want 4", caps.MaxN)
	}
	if caps.SupportsSeed || caps.SupportsImg2Img || caps.SupportsNegative {
		t.Errorf("unexpected capability flags: %+v", caps)
	}
}
