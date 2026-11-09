package automatic1111

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

// Generate must POST to /sdapi/v1/txt2img with the A1111-shaped body (prompt,
// negative_prompt, width/height parsed from Size, batch_size clamped, model as
// override_settings, plus Extra knobs), then decode the base64 images and
// recover per-image seeds from the JSON-encoded info string.
func TestGenerate_ShapesRequestAndDecodesImages(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	imgA := []byte("fake-png-A")
	imgB := []byte("fake-png-B")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		info := `{\"seed\":111,\"all_seeds\":[111,222]}`
		io.WriteString(w, `{"images":["`+
			base64.StdEncoding.EncodeToString(imgA)+`","`+
			base64.StdEncoding.EncodeToString(imgB)+`"],"info":"`+info+`"}`)
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "a1111", BaseURL: srv.URL, HTTPClient: srv.Client()})
	if c.Type() != "automatic1111" {
		t.Errorf("Type() = %q, want automatic1111", c.Type())
	}
	resp, err := c.Generate(context.Background(), imagegen.GenerateRequest{
		Prompt:   "a castle",
		Negative: "blurry",
		Size:     "768x1024",
		N:        2,
		Model:    "sd_xl_base_1.0",
		Extra: map[string]any{
			"steps":        float64(30),
			"sampler_name": "Euler a",
			"cfg_scale":    float64(7),
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if gotPath != "/sdapi/v1/txt2img" {
		t.Errorf("path = %q, want /sdapi/v1/txt2img", gotPath)
	}
	if gotBody["prompt"] != "a castle" || gotBody["negative_prompt"] != "blurry" {
		t.Errorf("prompt/negative wrong: %#v", gotBody)
	}
	if w, _ := gotBody["width"].(float64); w != 768 {
		t.Errorf("width = %v, want 768", gotBody["width"])
	}
	if h, _ := gotBody["height"].(float64); h != 1024 {
		t.Errorf("height = %v, want 1024", gotBody["height"])
	}
	if bs, _ := gotBody["batch_size"].(float64); bs != 2 {
		t.Errorf("batch_size = %v, want 2", gotBody["batch_size"])
	}
	if steps, _ := gotBody["steps"].(float64); steps != 30 {
		t.Errorf("steps = %v, want 30 (from Extra)", gotBody["steps"])
	}
	if gotBody["sampler_name"] != "Euler a" {
		t.Errorf("sampler_name = %v, want Euler a", gotBody["sampler_name"])
	}
	os, _ := gotBody["override_settings"].(map[string]any)
	if os["sd_model_checkpoint"] != "sd_xl_base_1.0" {
		t.Errorf("model not forwarded to override_settings: %#v", gotBody["override_settings"])
	}

	if len(resp.Images) != 2 {
		t.Fatalf("want 2 images, got %d", len(resp.Images))
	}
	if string(resp.Images[0].Bytes) != string(imgA) || string(resp.Images[1].Bytes) != string(imgB) {
		t.Error("image bytes not decoded correctly")
	}
	if resp.Images[0].Seed != 111 || resp.Images[1].Seed != 222 {
		t.Errorf("per-image seeds not recovered: %d, %d", resp.Images[0].Seed, resp.Images[1].Seed)
	}
	if resp.Images[0].ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", resp.Images[0].ContentType)
	}
}

// An explicit Seed is forwarded; a nil Seed becomes A1111's -1 "random" sentinel.
func TestGenerate_SeedHandling(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"images":[],"info":"{}"}`)
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "a1111", BaseURL: srv.URL, HTTPClient: srv.Client()})
	seed := int64(42)
	if _, err := c.Generate(context.Background(), imagegen.GenerateRequest{Prompt: "x", Seed: &seed}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if s, _ := gotBody["seed"].(float64); s != 42 {
		t.Errorf("explicit seed = %v, want 42", gotBody["seed"])
	}

	if _, err := c.Generate(context.Background(), imagegen.GenerateRequest{Prompt: "x"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if s, _ := gotBody["seed"].(float64); s != -1 {
		t.Errorf("nil seed should send -1, got %v", gotBody["seed"])
	}
}

// A blank prompt or invalid size fails before any network call.
func TestGenerate_ValidationErrors(t *testing.T) {
	c := New(imagegen.Options{Name: "a1111", BaseURL: "http://127.0.0.1:1"})
	if _, err := c.Generate(context.Background(), imagegen.GenerateRequest{Prompt: "  "}); err == nil {
		t.Error("want error on blank prompt")
	}
	if _, err := c.Generate(context.Background(), imagegen.GenerateRequest{Prompt: "x", Size: "notasize"}); err == nil {
		t.Error("want error on invalid size")
	}
}

// A non-200 from the WebUI surfaces the status and body.
func TestGenerate_APIErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "CUDA out of memory")
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "a1111", BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := c.Generate(context.Background(), imagegen.GenerateRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "CUDA out of memory") {
		t.Errorf("want the API error body surfaced, got %v", err)
	}
}

// ListModels reads /sdapi/v1/sd-models, using title as the canonical id and
// model_name as the display label.
func TestListModels_ParsesCheckpoints(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `[
			{"title":"sd_xl_base_1.0.safetensors [abc123]","model_name":"sd_xl_base_1.0"},
			{"title":"dreamshaper_8.safetensors","model_name":""}
		]`)
	}))
	defer srv.Close()

	c := New(imagegen.Options{Name: "a1111", BaseURL: srv.URL, HTTPClient: srv.Client()})
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/sdapi/v1/sd-models" {
		t.Errorf("listing path = %q, want /sdapi/v1/sd-models", gotPath)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if models[0].ID != "sd_xl_base_1.0.safetensors [abc123]" || models[0].DisplayName != "sd_xl_base_1.0" {
		t.Errorf("model 0 mapping wrong: %+v", models[0])
	}
	// Empty model_name falls back to the title for the display label.
	if models[1].DisplayName != "dreamshaper_8.safetensors" {
		t.Errorf("empty model_name should fall back to title, got %q", models[1].DisplayName)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in         string
		w, h       int
		wantErr    bool
		defW, defH int
	}{
		{"768x1024", 768, 1024, false, 512, 512},
		{"", 512, 512, false, 512, 512},
		{"auto", 640, 640, false, 640, 640},
		{"bad", 0, 0, true, 512, 512},
		{"0x100", 0, 0, true, 512, 512},
	}
	for _, tc := range cases {
		w, h, err := parseSize(tc.in, tc.defW, tc.defH)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSize(%q) want error", tc.in)
			}
			continue
		}
		if err != nil || w != tc.w || h != tc.h {
			t.Errorf("parseSize(%q) = %d,%d,%v; want %d,%d", tc.in, w, h, err, tc.w, tc.h)
		}
	}
}
