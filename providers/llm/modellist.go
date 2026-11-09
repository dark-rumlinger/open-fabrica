package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"openfabrica/internal/providercfg"
)

// This file holds every model-catalog parser the openai-chat transport can
// serve, keyed by providercfg.ModelsShape.
//
// They live here, in package llm, because the alternative was a Go package per
// vendor whose ONLY reason to exist was its listing payload, each one embedding
// *openai.Client and overriding nothing else. A response shape is a small,
// closed, testable thing;
// as a named constant rather than a type, a new vendor with a familiar catalog
// format is a config row, and one with a genuinely novel format is a parser here
// rather than a transport.
//
// The dividing line: request SHAPING and auth are data (providercfg), response
// PARSING is code (this file). Anything needing a different request body or wire
// protocol is a real transport; anthropic and ollama are the two that earn it.

// fallbackContextWindow is what a catalog reports when it tells us nothing
// about a model's limits. Deliberately conservative: overstating the window
// makes the engine send prompts the server silently truncates, which surfaces
// much later as a cache miss or a mangled continuation.
const fallbackContextWindow = 8192

// ModelListSpec is everything the shape parsers need to fetch and interpret a
// catalog. Constructed by the transport, which has already resolved the URLs
// from the provider row (see providerreg.Options.Endpoint / Sibling).
type ModelListSpec struct {
	HTTPClient *http.Client
	// URL is the catalog endpoint: providers.models_url when the row sets one,
	// otherwise derived from the base root.
	URL string
	// Shape selects the parser. Empty means providercfg.ShapeOpenAI.
	Shape string
	// Label names the provider in error messages.
	Label string
	// SetHeaders authenticates the request. May be nil for public catalogs.
	SetHeaders func(*http.Request)
	// StrictOpenAIFamilies restricts a ShapeOpenAI listing to OpenAI's own
	// families. Only api.openai.com wants it; every other vendor sharing the
	// envelope has its own naming scheme and would filter down to nothing.
	StrictOpenAIFamilies bool
	// RuntimeInfoURL is llama-server's /props. ShapeLlamaCpp only; empty
	// skips the refinement.
	RuntimeInfoURL string
}

// ListModels fetches spec.URL and parses it according to spec.Shape.
func ListModels(ctx context.Context, spec ModelListSpec) ([]ModelInfo, error) {
	shape := spec.Shape
	if shape == "" {
		shape = providercfg.ShapeOpenAI
	}
	switch shape {
	case providercfg.ShapeOpenAI:
		return listOpenAI(ctx, spec)
	case providercfg.ShapeOpenRouter:
		return listOpenRouter(ctx, spec)
	case providercfg.ShapeGemini:
		return listGemini(ctx, spec)
	case providercfg.ShapeOllama:
		return listOllama(ctx, spec)
	case providercfg.ShapeLlamaCpp:
		return listLlamaCpp(ctx, spec)
	default:
		return nil, fmt.Errorf("%s list models: unknown models_shape %q", spec.Label, shape)
	}
}

// WithQuery returns raw with one query parameter set, leaving the rest intact.
// Used to page a listing without disturbing a credential the caller appends
// later through the auth descriptor.
func WithQuery(raw, key, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// MaxListPages bounds a paginated listing so a server that keeps returning a
// next-page token cannot loop forever.
const MaxListPages = 20

// listOpenAI parses the plain OpenAI envelope: {"data":[{"id":...}]}. Shared by
// every vendor that copied the endpoint verbatim.
func listOpenAI(ctx context.Context, spec ModelListSpec) ([]ModelInfo, error) {
	type payloadT struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	payload, err := FetchJSON[payloadT](ctx, spec.HTTPClient, spec.URL, spec.Label+" list models", spec.SetHeaders)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(payload.Data))
	for _, m := range payload.Data {
		if !isChatModel(m.ID, spec.StrictOpenAIFamilies) {
			continue
		}
		out = append(out, ModelInfo{
			ID:          m.ID,
			DisplayName: m.ID,
			// No window here: the envelope reports only an id, so the number
			// comes from the name heuristic in FillFromName, which owns that
			// precedence in one place.
			SupportsStreaming: true,
			SupportsTools:     openAISupportsTools(m.ID),
			SupportsVision:    openAISupportsVision(m.ID),
		})
	}
	return out, nil
}

// listOpenRouter parses the aggregator catalog. Capability flags come from the
// response's architecture (modality) and supported_parameters fields rather
// than from a name heuristic, so they are actually trustworthy here. Pricing is
// reported per token as decimal strings and converted to USD per million.
func listOpenRouter(ctx context.Context, spec ModelListSpec) ([]ModelInfo, error) {
	type payloadT struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Architecture  struct {
				InputModalities []string `json:"input_modalities"`
				Modality        string   `json:"modality"`
			} `json:"architecture"`
			SupportedParameters []string `json:"supported_parameters"`
			TopProvider         struct {
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			Pricing struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"data"`
	}
	payload, err := FetchJSON[payloadT](ctx, spec.HTTPClient, spec.URL, spec.Label+" list models", spec.SetHeaders)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(payload.Data))
	for _, m := range payload.Data {
		display := m.Name
		if display == "" {
			display = m.ID
		}
		out = append(out, ModelInfo{
			ID:                m.ID,
			DisplayName:       display,
			ContextTokens:     m.ContextLength,
			MaxOutputTokens:   m.TopProvider.MaxCompletionTokens,
			SupportsStreaming: true, // OpenRouter streams every model
			SupportsTools:     slices.Contains(m.SupportedParameters, "tools"),
			SupportsVision: slices.Contains(m.Architecture.InputModalities, "image") ||
				strings.Contains(m.Architecture.Modality, "image"),
			SupportsReasoning: slices.Contains(m.SupportedParameters, "reasoning"),
			Cost: ModelCost{
				Input:      perMillion(m.Pricing.Prompt),
				Output:     perMillion(m.Pricing.Completion),
				CacheRead:  perMillion(m.Pricing.InputCacheRead),
				CacheWrite: perMillion(m.Pricing.InputCacheWrite),
			},
		})
	}
	return out, nil
}

// perMillion converts a per-token price string ("0.000003") to USD per million
// tokens. Empty, unparsable and negative values (a router's "-1" placeholder)
// are unknown and yield 0.
func perMillion(perToken string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(perToken), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v * 1e6
}

// listGemini parses Google's native /v1beta/models, keeping only entries that
// declare generateContent. The credential rides the query string, so this shape
// is normally paired with a models_auth of {scheme:"query", param:"key"}. The
// endpoint pages at 50 by default, so the request asks for the maximum page size
// and follows nextPageToken until it runs out.
func listGemini(ctx context.Context, spec ModelListSpec) ([]ModelInfo, error) {
	type payloadT struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			InputTokenLimit            int      `json:"inputTokenLimit"`
			OutputTokenLimit           int      `json:"outputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			Thinking                   bool     `json:"thinking"`
		} `json:"models"`
		NextPageToken string `json:"nextPageToken"`
	}
	var out []ModelInfo
	pageURL := WithQuery(spec.URL, "pageSize", "1000")
	for page := 0; page < MaxListPages; page++ {
		payload, err := FetchJSON[payloadT](ctx, spec.HTTPClient, pageURL, spec.Label+" list models", spec.SetHeaders)
		if err != nil {
			return nil, err
		}
		for _, m := range payload.Models {
			if !slices.Contains(m.SupportedGenerationMethods, "generateContent") {
				continue
			}
			id := strings.TrimPrefix(m.Name, "models/")
			display := m.DisplayName
			if display == "" {
				display = id
			}
			outputCap := m.OutputTokenLimit
			if outputCap == 0 {
				outputCap = 65536
			}
			out = append(out, ModelInfo{
				ID:                id,
				DisplayName:       display,
				ContextTokens:     m.InputTokenLimit,
				MaxOutputTokens:   outputCap,
				SupportsStreaming: true,
				SupportsTools:     true,
				SupportsVision:    geminiSupportsVision(id),
				SupportsReasoning: m.Thinking,
			})
		}
		if payload.NextPageToken == "" {
			break
		}
		pageURL = WithQuery(WithQuery(spec.URL, "pageSize", "1000"), "pageToken", payload.NextPageToken)
	}
	return out, nil
}

// listOllama parses GET /api/tags - Ollama's own listing rather than its
// OpenAI-compatible one, because /api/tags reports the pulled tag exactly as
// the daemon will accept it back. Capability flags use a coarse family-name
// heuristic; admins can flip per-model flags in the UI when it is wrong.
func listOllama(ctx context.Context, spec ModelListSpec) ([]ModelInfo, error) {
	type payloadT struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	payload, err := FetchJSON[payloadT](ctx, spec.HTTPClient, spec.URL, spec.Label+" list models", spec.SetHeaders)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(payload.Models))
	for _, m := range payload.Models {
		id := m.Name
		if id == "" {
			id = m.Model
		}
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:                id,
			DisplayName:       id,
			ContextTokens:     ollamaDefaultContext,
			SupportsStreaming: true,
			SupportsTools:     ollamaSupportsTools(id),
			// The vision family list is shared with llama.cpp: both runtimes
			// serve the same local-model ecosystem, so the lists must not drift.
			SupportsVision: MatchesAnyFamily(id, LocalVisionFamilies),
		})
	}
	return out, nil
}

// ollamaDefaultContext is Ollama's own default num_ctx. /api/tags reports no
// window, and the daemon truncates silently past this, so it is the honest
// number to publish rather than the larger trained window.
const ollamaDefaultContext = 4096

func ollamaSupportsTools(name string) bool {
	return MatchesAnyFamily(name, []string{"llama3.1", "llama3.2", "llama3.3", "qwen2.5", "qwen3", "mistral-nemo", "command-r"})
}

// listLlamaCpp reads each model's trained context window (meta.n_ctx_train) and
// the runtime window the server was started with (-c, via /props), and reports
// the usable minimum as MaxTokens: a 128k-trained model started with `-c 8192`
// holds only 8192 tokens before a context shift evicts the cached prefix, so
// the runtime cap is what callers must budget against. Either source missing
// falls back to the other, then to fallbackContextWindow.
//
// llama-server serves whatever model it was launched with regardless of the
// request's "model" field, so the id is mostly cosmetic, but echoing the
// server's own id keeps logs and the model picker honest.
func listLlamaCpp(ctx context.Context, spec ModelListSpec) ([]ModelInfo, error) {
	type payloadT struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtxTrain int `json:"n_ctx_train"`
			} `json:"meta"`
		} `json:"data"`
	}
	payload, err := FetchJSON[payloadT](ctx, spec.HTTPClient, spec.URL, spec.Label+" list models", spec.SetHeaders)
	if err != nil {
		return nil, err
	}
	runtimeCtx := llamaRuntimeContext(ctx, spec) // 0 when /props is unavailable
	out := make([]ModelInfo, 0, len(payload.Data))
	for _, m := range payload.Data {
		out = append(out, ModelInfo{
			ID:                m.ID,
			DisplayName:       ggufDisplayName(m.ID),
			ContextTokens:     usableContext(m.Meta.NCtxTrain, runtimeCtx),
			SupportsStreaming: true,
			// llama-server emits OpenAI tool calls when started with --jinja and
			// a tool-capable chat template. We can't see the server's flags from
			// here, so assume tool support (the build pipeline requires it) and
			// let the admin flip the per-model flag off when the template lacks it.
			SupportsTools: true,
			// Vision needs an mmproj projector loaded alongside the model, which
			// we can't observe here, so this is a coarse name heuristic.
			SupportsVision: MatchesAnyFamily(m.ID, LocalVisionFamilies),
		})
	}
	return out, nil
}

// llamaRuntimeContext queries llama-server's /props for the context window the
// process was started with. Returns 0 on any failure so listLlamaCpp falls back
// to the model's trained window; /props is best-effort enrichment, not a hard
// dependency.
func llamaRuntimeContext(ctx context.Context, spec ModelListSpec) int {
	if spec.RuntimeInfoURL == "" {
		return 0
	}
	type propsT struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	props, err := FetchJSON[propsT](ctx, spec.HTTPClient, spec.RuntimeInfoURL, spec.Label+" props", spec.SetHeaders)
	if err != nil {
		return 0
	}
	return props.DefaultGenerationSettings.NCtx
}

// usableContext returns the smaller of the model's trained window and the
// server's runtime window, ignoring zero (unknown) values.
func usableContext(trained, runtime int) int {
	switch {
	case trained > 0 && runtime > 0:
		return min(trained, runtime)
	case trained > 0:
		return trained
	case runtime > 0:
		return runtime
	default:
		return fallbackContextWindow
	}
}

// ggufDisplayName makes a GGUF path readable in the model picker: llama-server's
// id is often the on-disk path, so strip the directory and the .gguf suffix. A
// plain alias (set via -a) passes through unchanged.
func ggufDisplayName(id string) string {
	name := path.Base(strings.ReplaceAll(id, "\\", "/"))
	return strings.TrimSuffix(name, ".gguf")
}

// isChatModel filters an OpenAI-envelope listing. The `bad` substrings rule out
// shapes no chat endpoint accepts (embeddings, audio, image, etc.). When strict
// it also requires an OpenAI family prefix.
func isChatModel(id string, strict bool) bool {
	if MatchesAnyFamily(id, []string{"embedding", "tts", "whisper", "moderation", "image", "dall-e", "audio", "realtime", "transcribe", "babbage", "davinci"}) {
		return false
	}
	low := strings.ToLower(id)
	if strings.HasSuffix(low, "-instruct") {
		return false
	}
	if !strict {
		return true
	}
	return strings.HasPrefix(low, "gpt-") ||
		strings.HasPrefix(low, "chatgpt-") ||
		strings.HasPrefix(low, "o1") ||
		strings.HasPrefix(low, "o3") ||
		strings.HasPrefix(low, "o4")
}

func geminiSupportsVision(id string) bool {
	// Modern Gemini models are multimodal; exclude text-only and embedding
	// variants by name.
	if MatchesAnyFamily(id, []string{"embedding", "aqa", "text-bison"}) {
		return false
	}
	return strings.Contains(strings.ToLower(id), "gemini")
}

// The name heuristics below are the LAST resort for a listing that reports only
// an id (the plain OpenAI envelope): limits and capabilities are guessed from the
// model name. They never overwrite a number the provider reported; see
// FillFromName in model_sync.go for the precedence. The figures follow each
// vendor's published model specs and should be refreshed when a new family ships.

// nameLimits returns the context window and output cap implied by a model id.
// The window falls back to fallbackContextWindow and the cap to 0 (unknown).
func nameLimits(id string) (ctx, out int) {
	low := strings.ToLower(id)
	switch {
	case strings.HasPrefix(low, "gpt-"):
		return gptLimits(low)
	case strings.HasPrefix(low, "chatgpt-"):
		return 128000, 16384
	case strings.HasPrefix(low, "o1"), strings.HasPrefix(low, "o3"), strings.HasPrefix(low, "o4"):
		return 200000, 100000
	case strings.HasPrefix(low, "glm-"):
		return glmLimits(low)
	case strings.HasPrefix(low, "claude-"):
		// The Models API normally reports the window; 200k is the conservative
		// baseline for when it does not (a proxy or an older API version).
		return 200000, 0
	}
	for _, f := range familyLimits {
		if strings.HasPrefix(low, f.prefix) {
			return f.ctx, f.out
		}
	}
	return fallbackContextWindow, 0
}

// familyLimits are prefix rules for model families whose listing carries no
// limits. Ordered most specific first; the first match wins.
var familyLimits = []struct {
	prefix   string
	ctx, out int
}{
	{"k3-256k", 262144, 131072},
	{"k3", 1048576, 131072},
	{"kimi-for-coding", 262144, 32768},
	{"kimi-k2", 262144, 32768},
	{"minimax-m3", 1048576, 512000},
	{"minimax-m2", 204800, 131072},
	{"qwen3-coder-plus", 1000000, 65536},
	{"qwen3-coder", 262144, 65536},
	{"qwen3-max", 262144, 32768},
	{"qwen3.", 1000000, 65536},
}

// gptLimits covers the gpt-* families. Mini and nano variants and codex models
// keep the 400k window of the generation they belong to; the flagship of the
// 5.4 generation onward, and every 6.x model, carries a 1.05M window.
func gptLimits(low string) (int, int) {
	major, minor := leadingVersion(strings.TrimPrefix(low, "gpt-"))
	small := strings.Contains(low, "mini") || strings.Contains(low, "nano") || strings.Contains(low, "codex")
	switch {
	case strings.Contains(low, "-chat"):
		return 128000, 16384
	case major >= 6 || (major == 5 && minor >= 4):
		if small && major == 5 {
			return 400000, 128000
		}
		return 1050000, 128000
	case major == 5:
		return 400000, 128000
	case major == 4 && strings.HasPrefix(low, "gpt-4.1"):
		return 1047576, 32768
	case major == 4 && strings.HasPrefix(low, "gpt-4o"):
		return 128000, 16384
	case major == 4 && strings.Contains(low, "turbo"):
		return 128000, 4096
	case major == 4:
		return 8192, 8192
	case major == 3:
		return 16385, 4096
	default:
		return fallbackContextWindow, 0
	}
}

// leadingVersion reads a "major.minor" prefix off a model id remainder:
// "5.4-mini" is (5, 4), "4o" is (4, 0). Both are 0 when there is no number.
func leadingVersion(rest string) (major, minor int) {
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		major = major*10 + int(rest[i]-'0')
		i++
	}
	if i > 0 && i < len(rest) && rest[i] == '.' {
		i++
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			minor = minor*10 + int(rest[i]-'0')
			i++
		}
	}
	return major, minor
}

// glmLimits covers the GLM family: 5.2 and later carry a 1M window, the 4.6
// to 5.1 generation about 200k, and the 4.5 generation 128k.
func glmLimits(low string) (int, int) {
	major, minor := leadingVersion(strings.TrimPrefix(low, "glm-"))
	switch {
	case major >= 6 || (major == 5 && minor >= 2):
		return 1000000, 131072
	case major == 5 || (major == 4 && minor >= 6):
		return 200000, 131072
	case major == 4 && minor == 5:
		return 131072, 98304
	default:
		return fallbackContextWindow, 0
	}
}

func openAISupportsTools(id string) bool {
	return !strings.Contains(strings.ToLower(id), "chatgpt-")
}

func openAISupportsVision(id string) bool {
	low := strings.ToLower(id)
	if strings.Contains(low, "nano") {
		return false
	}
	if strings.HasPrefix(low, "glm-") {
		return glmSupportsVision(low)
	}
	return strings.HasPrefix(low, "gpt-5") ||
		strings.HasPrefix(low, "gpt-6") ||
		strings.HasPrefix(low, "gpt-4.1") ||
		strings.HasPrefix(low, "gpt-4o") ||
		strings.HasPrefix(low, "chatgpt-") ||
		strings.HasPrefix(low, "o1") ||
		strings.HasPrefix(low, "o3") ||
		strings.HasPrefix(low, "o4")
}

// glmSupportsVision reports the GLM vision variants, which carry a "v" right
// after the version number (glm-4.5v, glm-4.6v, glm-5v-turbo).
func glmSupportsVision(low string) bool {
	rest := strings.TrimPrefix(low, "glm-")
	i := 0
	for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
		i++
	}
	return i > 0 && i < len(rest) && rest[i] == 'v'
}
