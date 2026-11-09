package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// This file is the shared HTTP transport skeleton for provider clients. Every
// completion endpoint speaks the same POST-JSON dance (marshal, request with
// ctx, headers, execute, classify failures, decode or stream), and every
// catalog listing the same GET-JSON one; centralizing them keeps the error
// classification identical across transports. tag labels every error and MUST
// be the PROVIDER ROW NAME, not the transport id: providers.name is unique and
// most vendors share a transport, so "Google Gemini: rate limited" identifies
// the configured provider where "openai-chat: rate limited" would not.

// PostJSONStream marshals body, POSTs it to url, and returns the open 200
// response for the caller's SSE parser; the caller owns closing resp.Body.
// Transport and HTTP failures come back as typed *Error under tag so the
// retry layer can route on the code; a non-200 body is consumed by
// classification and closed here. setHeaders stamps provider-specific headers
// (auth, API version) after the Content-Type default; nil sends JSON only.
func PostJSONStream(ctx context.Context, hc *http.Client, url, tag string, body any, setHeaders func(*http.Request)) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal request: %w", tag, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: create request: %w", tag, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if setHeaders != nil {
		setHeaders(req)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, ClassifyTransportError(tag, err)
	}
	if resp.StatusCode != http.StatusOK {
		herr := ClassifyHTTPError(tag, resp)
		resp.Body.Close()
		return nil, herr
	}
	return resp, nil
}

// PostJSON is PostJSONStream's buffered twin: same POST and error
// classification, but the 200 reply is decoded into out and the body closed
// before returning.
func PostJSON(ctx context.Context, hc *http.Client, url, tag string, body any, setHeaders func(*http.Request), out any) error {
	resp, err := PostJSONStream(ctx, hc, url, tag, body, setHeaders)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode response: %w", tag, err)
	}
	return nil
}

// FetchJSON GETs url and decodes the 200 reply into T. It is the transport
// skeleton behind every provider's catalog listing: each client keeps its own
// payload type and ModelInfo mapping, only the GET/status/decode dance lives
// here. Errors are plain fmt-wrapped under tag rather than classified: listing
// runs from the admin sync flow, whose surface is the message, not a retry
// code. A non-200 reports the status plus the capped body text.
func FetchJSON[T any](ctx context.Context, hc *http.Client, url, tag string, setHeaders func(*http.Request)) (T, error) {
	var out T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, fmt.Errorf("%s: %w", tag, err)
	}
	if setHeaders != nil {
		setHeaders(req)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("%s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("%s: HTTP %d: %s", tag, resp.StatusCode, ReadErrorBody(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("%s: decode: %w", tag, err)
	}
	return out, nil
}
