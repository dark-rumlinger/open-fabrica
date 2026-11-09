// Package automatic1111 implements imagegen.Provider against the Automatic1111
// Stable Diffusion WebUI API (also spoken by Forge, reForge, SD.Next, and
// Fooocus-API, register one of those by setting base_url accordingly).
// Registered under the type identifier "automatic1111".
package automatic1111

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openfabrica/providers/imagegen"
	"openfabrica/providers/internal/providerbase"
)

const defaultBaseURL = "http://127.0.0.1:7860"

// Client implements imagegen.Provider for the A1111 API.
type Client struct {
	*providerbase.BaseClient
}

func init() {
	imagegen.RegisterProvider("automatic1111", func(o imagegen.Options) imagegen.Provider {
		return New(o)
	})
}

// New constructs an A1111 client. A1111 is keyless by default (and so are the
// forks), but a deployment behind a reverse proxy can still need a credential,
// so the auth descriptor is honoured when configured. The timeout is 5 minutes
// since slow GPUs at high step counts genuinely take that long.
func New(o imagegen.Options) *Client {
	return &Client{providerbase.NewBaseClient(o, providerbase.Defaults{BaseURL: defaultBaseURL}, 5*time.Minute)}
}

func (c *Client) Type() string { return "automatic1111" }

func (c *Client) Capabilities() imagegen.Capabilities {
	return imagegen.Capabilities{
		MaxN:             4,
		Sizes:            []string{"512x512", "768x768", "1024x1024", "768x1024", "1024x768"},
		SupportsNegative: true,
		SupportsSeed:     true,
		SupportsImg2Img:  true,
	}
}

// txt2imgRequest is the subset of A1111's /sdapi/v1/txt2img body we
// populate. The full API has dozens of fields; we expose the common set
// and let callers pass anything else through GenerateRequest.Extra. Field
// names match the A1111 contract exactly.
type txt2imgRequest struct {
	Prompt           string  `json:"prompt"`
	NegativePrompt   string  `json:"negative_prompt,omitempty"`
	Steps            int     `json:"steps,omitempty"`
	SamplerName      string  `json:"sampler_name,omitempty"`
	CFGScale         float64 `json:"cfg_scale,omitempty"`
	Width            int     `json:"width,omitempty"`
	Height           int     `json:"height,omitempty"`
	BatchSize        int     `json:"batch_size,omitempty"`
	Seed             int64   `json:"seed,omitempty"`
	OverrideSettings struct {
		SDModelCheckpoint string `json:"sd_model_checkpoint,omitempty"`
	} `json:"override_settings,omitempty"`
}

type txt2imgResponse struct {
	Images []string `json:"images"` // base64 PNGs
	// Info is JSON-encoded as a string by A1111 (not a nested object).
	// Contains the actual seed used, sampler params, etc.
	Info string `json:"info"`
}

type infoBlob struct {
	Seed     int64   `json:"seed"`
	AllSeeds []int64 `json:"all_seeds"`
}

// Generate calls /sdapi/v1/txt2img. Width/height default to 512x512, the
// SD1.5 native resolution, which works on every base model. Larger sizes
// require the model and VRAM to support it, so we don't auto-upgrade.
func (c *Client) Generate(ctx context.Context, req imagegen.GenerateRequest) (*imagegen.GenerateResponse, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("automatic1111: prompt is required")
	}

	width, height, err := parseSize(req.Size, 512, 512)
	if err != nil {
		return nil, fmt.Errorf("automatic1111: %w", err)
	}

	n := req.N
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}

	body := txt2imgRequest{
		Prompt:         req.Prompt,
		NegativePrompt: req.Negative,
		Width:          width,
		Height:         height,
		BatchSize:      n,
	}
	if req.Seed != nil {
		body.Seed = *req.Seed
	} else {
		body.Seed = -1 // A1111 convention: -1 means "random"
	}
	if req.Model != "" {
		body.OverrideSettings.SDModelCheckpoint = req.Model
	}

	// Pull provider-specific knobs out of Extra. Unknown keys are ignored
	// here, A1111 accepts a permissive body, so callers wanting to pass
	// raw fields can fall back to the typed struct's defaults and let A1111
	// reject anything truly invalid.
	if v, ok := extraInt(req.Extra, "steps"); ok {
		body.Steps = v
	}
	if v, ok := extraString(req.Extra, "sampler_name"); ok {
		body.SamplerName = v
	}
	if v, ok := extraFloat(req.Extra, "cfg_scale"); ok {
		body.CFGScale = v
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("automatic1111: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL()+"/sdapi/v1/txt2img", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("automatic1111: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.Authorize(httpReq)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("automatic1111: send (is the WebUI running with --api at %s?): %w", c.BaseURL(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("automatic1111: API %d: %s", resp.StatusCode, string(raw))
	}

	var apiResp txt2imgResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("automatic1111: decode: %w", err)
	}

	// Parse the info blob to recover per-image seeds. info is a JSON-
	// encoded string (not a nested object) per the A1111 contract.
	var info infoBlob
	if apiResp.Info != "" {
		_ = json.Unmarshal([]byte(apiResp.Info), &info)
	}

	out := &imagegen.GenerateResponse{Images: make([]imagegen.GeneratedImage, 0, len(apiResp.Images))}
	for i, b64 := range apiResp.Images {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("automatic1111: decode image %d: %w", i, err)
		}
		seed := info.Seed
		if i < len(info.AllSeeds) {
			seed = info.AllSeeds[i]
		}
		out.Images = append(out.Images, imagegen.GeneratedImage{
			Bytes:       raw,
			ContentType: "image/png",
			Seed:        seed,
		})
	}
	return out, nil
}

// ListModels calls GET /sdapi/v1/sd-models and returns the checkpoints
// installed on the A1111 instance. A1111 lacks per-model metadata beyond
// title/name, so we surface the file's title as the canonical id (matches
// what override_settings.sd_model_checkpoint expects) and model_name as
// the display label. Returns an error when the WebUI is unreachable so
// the admin gets a clear "is --api running?" hint rather than an empty
// list.
func (c *Client) ListModels(ctx context.Context) ([]imagegen.ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+"/sdapi/v1/sd-models", nil)
	if err != nil {
		return nil, fmt.Errorf("automatic1111 list models: %w", err)
	}
	c.Authorize(httpReq)
	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("automatic1111 list models (is the WebUI running with --api at %s?): %w", c.BaseURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("automatic1111 list models: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var payload []struct {
		Title     string `json:"title"`
		ModelName string `json:"model_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("automatic1111 list models: decode: %w", err)
	}
	out := make([]imagegen.ModelInfo, 0, len(payload))
	for _, m := range payload {
		display := m.ModelName
		if display == "" {
			display = m.Title
		}
		out = append(out, imagegen.ModelInfo{ID: m.Title, DisplayName: display})
	}
	return out, nil
}

// parseSize accepts "WxH" (e.g. "768x1024"), "auto", or empty (defaults).
// A1111 has no concept of "auto", we map it to the supplied defaults.
func parseSize(s string, defW, defH int) (int, int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "auto" {
		return defW, defH, nil
	}
	parts := strings.Split(s, "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid size %q (want WxH)", s)
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid size %q", s)
	}
	return w, h, nil
}

func extraInt(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func extraFloat(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

func extraString(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok && v != ""
}
