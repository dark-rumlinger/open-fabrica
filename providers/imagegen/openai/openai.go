// Package openai implements the imagegen.Provider interface against
// OpenAI's images API. Default model is gpt-image-2; gpt-image-1.5 and
// gpt-image-1-mini are the cheaper tiers, and gpt-image-1 is the budget
// option. Registered under the type identifier "openai-image" so it's
// distinct from the chat provider also named "openai".
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openfabrica/providers/imagegen"
	"openfabrica/providers/internal/providerbase"
)

// defaultBaseURL is an API ROOT, not an operation endpoint: every provider
// base URL in the system names the root and the transport appends the path.
const defaultBaseURL = "https://api.openai.com/v1"

// Client implements imagegen.Provider for OpenAI's image-generation API.
type Client struct {
	*providerbase.BaseClient
	endpoint  string
	modelsURL string
}

func init() {
	imagegen.RegisterProvider("openai-image", func(o imagegen.Options) imagegen.Provider {
		return New(o)
	})
}

// New constructs an OpenAI image-gen client. Options.BaseURL is optional and
// empty falls back to api.openai.com. Image generation can take 30+ seconds at
// the high quality tier, so the default timeout is generous.
func New(o imagegen.Options) *Client {
	modelsURL := o.ModelsURL
	if modelsURL == "" {
		modelsURL = o.Endpoint(defaultBaseURL, "models")
	}
	return &Client{
		BaseClient: providerbase.NewBaseClient(o, providerbase.Defaults{BaseURL: defaultBaseURL}, 3*time.Minute),
		endpoint:   o.Endpoint(defaultBaseURL, "images", "generations"),
		modelsURL:  modelsURL,
	}
}

func (c *Client) Type() string { return "openai-image" }

func (c *Client) Capabilities() imagegen.Capabilities {
	return imagegen.Capabilities{
		MaxN:             4,
		Sizes:            []string{"1024x1024", "1024x1792", "1792x1024", "auto"},
		SupportsNegative: false,
		SupportsSeed:     false,
		SupportsImg2Img:  false,
	}
}

type apiRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type apiResponse struct {
	Data []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
}

type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Generate calls /v1/images/generations and returns the decoded PNG bytes.
// gpt-image-1 always returns base64; response_format is set explicitly so
// DALL-E 3 also returns inline bytes instead of a URL (URL mode would
// force us to do a second fetch with its own SSRF/size concerns; b64
// keeps the trust boundary at OpenAI).
func (c *Client) Generate(ctx context.Context, req imagegen.GenerateRequest) (*imagegen.GenerateResponse, error) {
	if c.APIKey() == "" {
		return nil, fmt.Errorf("openai-image: no API key configured")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("openai-image: prompt is required")
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		// Adapter is the API boundary, not a defaulting layer. The tool
		// layer is responsible for resolving the provider's default model
		// from the DB; reaching here means the caller bypassed that and
		// we'd otherwise silently call whatever model id we hardcode. Fail
		// loud so the misuse surfaces.
		return nil, fmt.Errorf("openai-image: model is required (caller should resolve default from llm_models.is_default)")
	}
	n := req.N
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}

	body := apiRequest{
		Model:   model,
		Prompt:  req.Prompt,
		N:       n,
		Size:    req.Size,
		Quality: mapQuality(req.Quality),
		// response_format intentionally omitted: every gpt-image-* model
		// returns base64 by default and rejects the field.
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-image: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("openai-image: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.Authorize(httpReq)

	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-image: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("openai-image: decode: %w", err)
	}

	out := &imagegen.GenerateResponse{Images: make([]imagegen.GeneratedImage, 0, len(apiResp.Data))}
	for _, d := range apiResp.Data {
		if d.B64JSON == "" {
			// gpt-image-1-mini in URL mode would return a URL. We force b64
			// above; this branch only fires if OpenAI changes the contract ,
			// surface clearly instead of dropping silently.
			return nil, fmt.Errorf("openai-image: response missing b64_json (got URL %q)", d.URL)
		}
		raw, err := base64.StdEncoding.DecodeString(d.B64JSON)
		if err != nil {
			return nil, fmt.Errorf("openai-image: decode b64: %w", err)
		}
		out.Images = append(out.Images, imagegen.GeneratedImage{
			Bytes:       raw,
			ContentType: "image/png",
			Revised:     d.RevisedPrompt,
		})
	}
	return out, nil
}

// mapQuality translates the imagegen-normalized quality value into OpenAI's
// gpt-image-* vocabulary (low/medium/high).
func mapQuality(q string) string {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case "low":
		return "low"
	case "high":
		return "high"
	case "", "standard", "medium":
		return "medium"
	default:
		return "medium"
	}
}

// ListModels hits the configured models URL and filters to image-generation
// families (gpt-image-*, dall-e-*). The URL comes from the provider row when
// set and is otherwise derived from the base root, so a self-hosted compatible
// proxy that lists on an unrelated path is configuration rather than code.
// Returns an error on non-200; the admin sync handler surfaces it as a clean
// toast rather than silently producing an empty list.
func (c *Client) ListModels(ctx context.Context) ([]imagegen.ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("openai-image list models: %w", err)
	}
	c.Authorize(httpReq)
	resp, err := c.HTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-image list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("openai-image list models: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("openai-image list models: decode: %w", err)
	}
	out := make([]imagegen.ModelInfo, 0, len(payload.Data))
	for _, m := range payload.Data {
		low := strings.ToLower(m.ID)
		if !strings.HasPrefix(low, "gpt-image") && !strings.HasPrefix(low, "dall-e") {
			continue
		}
		out = append(out, imagegen.ModelInfo{ID: m.ID, DisplayName: m.ID})
	}
	return out, nil
}

func (c *Client) readError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e apiError
	if err := json.Unmarshal(body, &e); err == nil && e.Error.Message != "" {
		return fmt.Errorf("openai-image: API %d: %s: %s", resp.StatusCode, e.Error.Type, e.Error.Message)
	}
	return fmt.Errorf("openai-image: API %d: %s", resp.StatusCode, string(body))
}
