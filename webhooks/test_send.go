package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"openfabrica/internal/netguard"
	"openfabrica/security"
)

// testSendTimeout bounds one test delivery. The admin is waiting on the
// answer, so it is shorter than a real delivery's 15s client timeout.
const testSendTimeout = 10 * time.Second

// TestResult is the outcome of a single test delivery, shaped for the admin UI.
type TestResult struct {
	Success    bool   `json:"success"`
	StatusCode int    `json:"status_code,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	// Response is the first part of the receiver's reply body, to help debug a
	// rejection.
	Response string `json:"response,omitempty"`
}

// SendTest posts one signed "test" event to url and reports what came back.
// It goes through the same SSRF-hardened client and signature scheme as a real
// delivery, so a pass here means a real event will reach the receiver and
// verify. Unlike a real delivery it makes a single attempt: the admin wants the
// answer now, not after retry backoff.
func SendTest(ctx context.Context, url, secret string, projectID int, name string) TestResult {
	client := netguard.SSRFSafeClient()
	client.Timeout = testSendTimeout
	return sendTest(ctx, client, true, url, secret, projectID, name)
}

// sendTest is SendTest with the client and SSRF pre-flight injectable, so tests
// can deliver to a loopback httptest server.
func sendTest(ctx context.Context, client *http.Client, validate bool, url, secret string, projectID int, name string) TestResult {
	start := time.Now()
	fail := func(msg string) TestResult {
		return TestResult{Error: msg, DurationMs: time.Since(start).Milliseconds()}
	}

	if validate {
		if err := netguard.ValidateExternalURL(url); err != nil {
			return fail("This URL is not allowed: " + err.Error())
		}
	}

	payload, err := json.Marshal(map[string]any{
		"event":      "test",
		"project_id": projectID,
		"payload":    map[string]any{"webhook": name, "message": "This is a test event from Open Fabrica."},
		"timestamp":  time.Now().UTC(),
	})
	if err != nil {
		return fail("could not build the test payload")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fail("The URL could not be used: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenFabrica-Webhook/1.0")
	if secret != "" {
		req.Header.Set("X-Webhook-Signature", "sha256="+security.HMACSHA256Hex([]byte(secret), payload))
	}

	resp, err := client.Do(req)
	if err != nil {
		return fail(err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	res := TestResult{
		StatusCode: resp.StatusCode,
		DurationMs: time.Since(start).Milliseconds(),
		Response:   string(body),
	}
	res.Success = resp.StatusCode >= 200 && resp.StatusCode < 300
	return res
}
