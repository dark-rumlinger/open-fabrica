package probe

import (
	"context"
	"fmt"
	"testing"
	"time"

	"openfabrica/store"
)

func noChrome(context.Context, store.BrowserConfig) (context.Context, context.CancelFunc, error) {
	return nil, nil, fmt.Errorf("%w: exec: \"chrome\": executable file not found", store.ErrBrowserUnavailable)
}

// Without Chrome the run is skipped: no issues, no pass/fail verdicts.
func TestRunFlowTests_NoChromeIsASkip(t *testing.T) {
	defer SetFlowBrowserLauncherForTest(noChrome)()
	suite, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [{ goto: "/" }] }] }`)
	if err != nil {
		t.Fatal(err)
	}
	res := RunFlowTests(context.Background(), suite, "http://127.0.0.1:1", "", "")
	if res.Skipped == "" || len(res.Issues) != 0 || len(res.Passed)+len(res.Failed) != 0 {
		t.Fatalf("want a clean skip, got %+v", res)
	}
}

// A caller cancelling mid-flow (the stage timed out) is a skip too, not a
// "flow ran out of time" error that would drive a fix-pass.
func TestRunFlowTests_CancelledRunIsASkip(t *testing.T) {
	base := flowSite(t)
	if _, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{}); err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	} else {
		cancel()
	}
	suite, err := ParseFlowSource(`export default { flows: [{ name: "slow", steps: [
		{ goto: "/dashboard" },
		{ expectText: "never shown", timeout: 30000 },
	] }] }`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(6*time.Second, cancel)
	res := RunFlowTests(ctx, suite, base, "", "")
	if res.Skipped == "" || len(res.Issues) != 0 {
		t.Fatalf("want a skip after cancellation, got skipped=%q issues=%+v", res.Skipped, res.Issues)
	}
}
