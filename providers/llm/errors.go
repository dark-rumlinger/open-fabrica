package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxErrorBody bounds how much of an error response body is read when building a
// provider error message. Provider error bodies are small JSON objects; a
// misbehaving or proxied endpoint could stream far more, so the read is capped.
const maxErrorBody = 64 << 10 // 64 KiB

// ReadErrorBody reads up to maxErrorBody bytes of an error response body and
// returns it as a string, best-effort. It never returns an error: the caller is
// already on a failure path, so an unreadable body just yields "". The body is
// consumed but not closed - the caller still owns closing resp.Body.
func ReadErrorBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return string(body)
}

// ClassifyHTTPError converts an HTTP response from an LLM provider into a
// typed *Error with a Code the retry layer can route on. Callers pass the
// response (with status != 200) plus the provider identifier used in the
// outer message. The body is consumed.
func ClassifyHTTPError(provider string, resp *http.Response) *Error {
	text := ReadErrorBody(resp)
	code := codeFromStatus(resp.StatusCode, text)
	msg := fmt.Sprintf("%s: API error %d: %s", provider, resp.StatusCode, text)
	retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After"))
	// A quota error carries its reset moment in the body, not the Retry-After
	// header, and that reset can be hours out. Capture it uncapped (the header
	// parser caps at 5m, which is meant for short throttles) so the pause path
	// can tell the owner when work will resume.
	if code == CodeQuotaExceeded && retryAfter == 0 {
		retryAfter = parseQuotaReset(text)
	}
	return &Error{
		Code:       code,
		Status:     resp.StatusCode,
		Message:    msg,
		RetryAfter: retryAfter,
	}
}

// ClassifyTransportError wraps a non-HTTP error (DNS, connection refused,
// TLS, context-deadline) into a typed *Error. Network timeouts are mapped
// to CodeTimeout so the retry layer treats them as transient; everything
// else becomes CodeTransport.
func ClassifyTransportError(provider string, err error) *Error {
	if err == nil {
		return nil
	}
	code := CodeTransport
	if errors.Is(err, context.DeadlineExceeded) {
		code = CodeTimeout
	} else if errors.Is(err, context.Canceled) {
		code = CodeTimeout
	} else {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			code = CodeTimeout
		}
	}
	return &Error{
		Code:    code,
		Message: fmt.Sprintf("%s: %s", provider, err.Error()),
		Cause:   err,
	}
}

func codeFromStatus(status int, body string) string {
	switch {
	case status == http.StatusTooManyRequests, status == 529:
		if isQuotaExceeded(body) {
			return CodeQuotaExceeded
		}
		return CodeRateLimit
	case status == http.StatusPaymentRequired:
		// Out of credits: lasts until the account is topped up, so it pauses the
		// build like any quota lockout instead of being retried as a server fault.
		return CodeQuotaExceeded
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return CodeAuth
	case status == http.StatusRequestTimeout:
		return CodeTimeout
	case status == http.StatusNotFound, status == http.StatusRequestEntityTooLarge,
		status == http.StatusUnprocessableEntity, status == http.StatusMethodNotAllowed:
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed && isContextOverflow(body) {
			return CodeContextOverflow
		}
		// The same payload fails the same way (unknown model or route, body too
		// large, rejected field), so retrying only burns the backoff. 422 is also
		// how pydantic-based servers reject an unknown field, which the quirk
		// learners recognise on CodeBadRequest.
		if isSchemaRejection(body) {
			return CodeSchemaUnsupported
		}
		return CodeBadRequest
	case status == http.StatusBadRequest:
		if isContextOverflow(body) {
			return CodeContextOverflow
		}
		if isSchemaRejection(body) {
			return CodeSchemaUnsupported
		}
		return CodeBadRequest
	case status >= 500:
		if strings.Contains(strings.ToLower(body), "overloaded") {
			return CodeOverloaded
		}
		return CodeServer
	}
	return CodeServer
}

// isContextOverflow reports whether a 400/413 body says the PROMPT was too large for
// the model's context window. That is a property of the conversation, not of the
// request's shape: the caller's remedy is a smaller context (a rollover), where every
// other bad request needs the request itself changed. Matched on the phrases the major
// vendors and OpenAI-compatible servers use; a bare "too large" is not enough because
// request-body and file-size rejections say that too.
func isContextOverflow(body string) bool {
	low := strings.ToLower(body)
	for _, phrase := range []string{
		"context_length_exceeded",
		"maximum context length",
		"context window",
		"context length",
		"prompt is too long",
		"input is too long",
		"too many tokens",
		"exceeds the maximum number of tokens",
		"reduce the length of the messages",
	} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}

// isSchemaRejection reports whether a 400 body is the endpoint refusing a
// structured-output REQUEST FIELD, which routes to CodeSchemaUnsupported so the
// json_schema -> json_object -> none ladder and the provider fallback can act.
//
// The named fields only. A bare "schema" match was too broad: it swallowed any
// 400 that merely mentioned a schema, including Anthropic's "input_schema does
// not support oneOf..." - a complaint about the schema's CONTENT. Those must
// stay CodeBadRequest, because the fix is to reshape the request and because
// every provider-quirk learner is gated on CodeBadRequest.
func isSchemaRejection(body string) bool {
	low := strings.ToLower(body)
	return strings.Contains(low, "response_format") ||
		strings.Contains(low, "json_schema") ||
		strings.Contains(low, "json schema") ||
		strings.Contains(low, "response_schema") ||
		strings.Contains(low, "structured output")
}

// isQuotaExceeded distinguishes a longer-lived account/plan quota lockout from
// a transient per-window 429. A quota error names a quota or usage allowance
// that resets on its own clock (often hours out), so retrying inside the
// rate-limit budget can never recover; the build pauses instead.
func isQuotaExceeded(body string) bool {
	low := strings.ToLower(body)
	// A per-minute or per-second allowance that names its own retry delay clears
	// on its own, so it stays a rate limit even when the body says "quota".
	for _, transient := range []string{"perminute", "per minute", "persecond", "per second", "retrydelay", "retry in"} {
		if strings.Contains(low, transient) {
			return false
		}
	}
	for _, marker := range []string{
		"quota", "billing", "insufficient_quota", "insufficient balance",
		"insufficient credit", "insufficient funds", "credit balance", "out of credits",
		"usage limit", "exceeded your current",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// parseQuotaReset best-effort extracts a reset timestamp from a quota error
// body and returns the duration until then. Providers phrase this as
// "reset at 2025-01-02 15:04:05 +0800 CST"; the trailing zone abbreviation is
// ignored since the numeric offset is authoritative. Returns 0 when no
// recognizable timestamp is present so the caller falls back to a generic
// message rather than a bogus countdown.
func parseQuotaReset(body string) time.Duration {
	m := quotaResetRe.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	t, err := time.Parse("2006-01-02 15:04:05 -0700", m[1])
	if err != nil {
		return 0
	}
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	return d
}

var quotaResetRe = regexp.MustCompile(`reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4})`)

func parseRetryAfterHeader(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		if secs > 300 {
			secs = 300
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		if d > 5*time.Minute {
			return 5 * time.Minute
		}
		return d
	}
	return 0
}
