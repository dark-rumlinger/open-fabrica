package llm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type flakyProvider struct {
	name string
	err  error
}

func (f *flakyProvider) Name() string                       { return f.name }
func (f *flakyProvider) Type() string                       { return "stub-transport" }
func (f *flakyProvider) Ping(context.Context, string) error { return f.err }
func (f *flakyProvider) Complete(context.Context, CompletionRequest) (*CompletionResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &CompletionResponse{Content: "ok"}, nil
}
func (f *flakyProvider) Stream(context.Context, CompletionRequest, StreamCallback) error {
	return f.err
}

func TestProviderHealthTracksRealCalls(t *testing.T) {
	t.Cleanup(func() { ResetHealth("") })
	if _, ok := HealthOf("flaky"); ok {
		t.Fatal("a provider nothing has called has no health record")
	}
	inner := &flakyProvider{name: "flaky"}
	lp := NewLoggedProvider(inner, &captureLogger{}, "chat", "", nil, nil)

	if _, err := lp.Complete(context.Background(), CompletionRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	h, ok := HealthOf("flaky")
	if !ok || h.LastOK.IsZero() || h.Consecutive != 0 || h.TotalOK != 1 {
		t.Fatalf("after a success: %+v", h)
	}

	inner.err = ClassifyHTTPError("openai-chat", &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Body:       http.NoBody,
		Header:     http.Header{},
	})
	for i := 0; i < 3; i++ {
		_, _ = lp.Complete(context.Background(), CompletionRequest{Model: "m"})
	}
	h, _ = HealthOf("flaky")
	if h.Consecutive != 3 || h.TotalFailure != 3 || h.LastError == "" || h.LastErrCode != CodeQuotaExceeded {
		t.Fatalf("after three failures: %+v", h)
	}

	inner.err = nil
	_, _ = lp.Complete(context.Background(), CompletionRequest{Model: "m"})
	if h, _ = HealthOf("flaky"); h.Consecutive != 0 || h.LastError == "" {
		t.Fatalf("a success clears the streak but keeps the last error for context: %+v", h)
	}
}

func TestProviderHealthIgnoresCallerCancellationAndBoundsMessages(t *testing.T) {
	t.Cleanup(func() { ResetHealth("") })
	RecordProviderCall("p", context.Canceled)
	if _, ok := HealthOf("p"); ok {
		t.Error("a cancelled call is the caller giving up, not the provider failing")
	}
	RecordProviderCall("p", errors.New(strings.Repeat("x", 5000)))
	if h, _ := HealthOf("p"); len(h.LastError) > maxHealthError+3 {
		t.Errorf("stored error is %d bytes", len(h.LastError))
	}
}
