package engine

// Unified retry policy. Every LLM call and every JSON-schema parse routes
// through StagePolicy.Run, so retry behaviour is configured per stage in
// one place and classification is driven by typed *llm.Error.Code values,
// not by scraping error strings.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"openfabrica/providers/llm"
)

// Policy is the retry budget for a single error class. MaxAttempts of 0 or
// 1 means no retry; the call is made exactly once.
type Policy struct {
	MaxAttempts int
	Backoff     func(attempt int) time.Duration
}

// StagePolicy groups the three budgets a stage cares about. Transient,
// RateLimit, and Timeout govern the LLM provider call.
type StagePolicy struct {
	Transient Policy
	RateLimit Policy
	Timeout   Policy
	// MaxTotalWait caps the backoff one call may sleep in AGGREGATE, across
	// every class. The per-class attempt counts bound each kind of failure on its
	// own but not their sum: rate-limit alone is 6 attempts at 15s to 180s, and
	// the fallback provider then re-runs the same budget, so a single turn could
	// absorb minutes with nothing but a large duration_ms to show for it. Zero
	// disables the ceiling.
	MaxTotalWait time.Duration
}

// callStats reports what one call actually did: provider attempts made, time
// spent sleeping in backoff, and whether the fallback answered. Recorded because
// the measured duration alone cannot tell a slow provider from a throttled one.
type callStats struct {
	Attempts     int
	Waited       time.Duration
	UsedFallback bool
}

// add merges a second leg (the fallback attempt) into the first.
func (c *callStats) add(o callStats) {
	c.Attempts += o.Attempts
	c.Waited += o.Waited
}

// FallbackHook is called once a stage exhausts its primary-provider budget.
// Returning a non-nil llm.Provider asks the StagePolicy to retry the call
// on the alternate; returning nil bubbles the original error up.
type FallbackHook func(err error) (alt llm.Provider, altModel string)

// defaultStagePolicy is the conservative budget every stage shares. The stage-named
// vars below are COPIES, each an override point for its own stage, so tuning one stage
// is a one-line change here. Unexported because nothing outside this package sets a
// stage's budget.
var defaultStagePolicy = StagePolicy{
	Transient: Policy{MaxAttempts: 3, Backoff: expBackoff(5*time.Second, 3, 60*time.Second)},
	RateLimit: Policy{MaxAttempts: 6, Backoff: expBackoff(15*time.Second, 2, 180*time.Second)},
	Timeout:   Policy{MaxAttempts: 3, Backoff: expBackoff(5*time.Second, 3, 60*time.Second)},
	// Five minutes of aggregate backoff is well past any healthy throttle and
	// still short of a stage timeout, so a wedged provider gives up while a
	// briefly rate-limited one still rides it out.
	MaxTotalWait: 5 * time.Minute,
}

var (
	policyPlan  = defaultStagePolicy
	policyBuild = defaultStagePolicy
	policyLive  = defaultStagePolicy
)

// CallLLM invokes a single completion under the stage's transient/rate-limit/
// timeout budgets. Errors lacking a typed llm.Error Code are treated as fatal so
// unknown failures surface loudly instead of being silently retried. onFallback,
// when non-nil, is invoked once after primary exhaustion to optionally swap to
// an alternate provider for one final attempt.
func (sp StagePolicy) CallLLM(
	ctx context.Context,
	logger interface {
		Warn(msg string, args ...any)
		Info(msg string, args ...any)
	},
	primary llm.Provider,
	req llm.CompletionRequest,
	timeout time.Duration,
	onFallback FallbackHook,
) (*llm.CompletionResponse, callStats, error) {
	resp, stats, err := sp.callOnProvider(ctx, logger, primary, req, timeout)
	if err == nil {
		return resp, stats, nil
	}
	if ctx.Err() != nil {
		return nil, stats, err
	}
	if onFallback == nil {
		return nil, stats, err
	}
	alt, altModel := onFallback(err)
	if alt == nil {
		return nil, stats, err
	}
	if logger != nil {
		logger.Warn("primary provider exhausted, attempting fallback",
			"primary_error", err.Error(), "fallback_provider", alt.Name(), "fallback_model", altModel)
	}
	fbReq := req
	if altModel != "" {
		fbReq.Model = altModel
	}
	// Fallback uses the same StagePolicy so a slow alternate doesn't burn
	// the whole stage. It does NOT recurse into another fallback hook,
	// avoiding ping-pong between two unhealthy providers.
	// The fallback inherits whatever wait the primary already burned, so the two
	// legs share one aggregate ceiling instead of each getting a fresh budget.
	fbPolicy := sp
	if fbPolicy.MaxTotalWait > 0 {
		if remaining := fbPolicy.MaxTotalWait - stats.Waited; remaining > 0 {
			fbPolicy.MaxTotalWait = remaining
		} else {
			fbPolicy.MaxTotalWait = 1 // exhausted: one attempt, no sleeping
		}
	}
	fbResp, fbStats, fbErr := fbPolicy.callOnProvider(ctx, logger, alt, fbReq, timeout)
	stats.add(fbStats)
	stats.UsedFallback = true
	if fbErr != nil {
		if logger != nil {
			logger.Warn("fallback provider also failed", "error", fbErr.Error())
		}
		// Surface both errors so operators see the alternate also failed
		// rather than only the primary's complaint. Double %w preserves
		// errors.Is/As semantics for typed *llm.Error checks on either side.
		return nil, stats, fmt.Errorf("primary=[%s]: %w; fallback=[%s]: %w",
			primary.Name(), err, alt.Name(), fbErr)
	}
	if logger != nil {
		logger.Info("fallback provider succeeded", "provider", alt.Name(), "model", altModel)
	}
	return fbResp, stats, nil
}

func (sp StagePolicy) callOnProvider(
	ctx context.Context,
	logger interface {
		Warn(msg string, args ...any)
		Info(msg string, args ...any)
	},
	provider llm.Provider,
	req llm.CompletionRequest,
	timeout time.Duration,
) (*llm.CompletionResponse, callStats, error) {
	transientAttempt := 0
	rateLimitAttempt := 0
	timeoutAttempt := 0
	var stats callStats
	for {
		callCtx := ctx
		var cancel context.CancelFunc
		if timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		stats.Attempts++
		resp, err := provider.Complete(callCtx, req)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return resp, stats, nil
		}
		if ctx.Err() != nil {
			return nil, stats, err
		}

		lerr, ok := llm.AsLLMError(err)
		if !ok {
			// Untyped error: treat as fatal so we surface protocol bugs
			// instead of silently retrying.
			return nil, stats, err
		}
		var (
			backoff time.Duration
			policy  Policy
			counter *int
		)
		switch lerr.Code {
		case llm.CodeRateLimit, llm.CodeOverloaded:
			policy = sp.RateLimit
			counter = &rateLimitAttempt
			if lerr.RetryAfter > 0 {
				backoff = lerr.RetryAfter
			}
		case llm.CodeTimeout:
			policy = sp.Timeout
			counter = &timeoutAttempt
		case llm.CodeServer, llm.CodeTransport:
			policy = sp.Transient
			counter = &transientAttempt
		case llm.CodeAuth, llm.CodeBadRequest, llm.CodeContextOverflow, llm.CodeSchemaUnsupported, llm.CodeTruncated, llm.CodeSchemaDrift, llm.CodeQuotaExceeded:
			// Fatal on this provider: retrying the same payload fails the same way.
			// Schema drift bubbles so CallLLM can hand it to the fallback hook;
			// quota exhaustion resets on the provider's own clock, so bubble up
			// and let the worker pause with the reset time.
			return nil, stats, err
		default:
			return nil, stats, err
		}
		*counter++
		if *counter >= policy.MaxAttempts {
			return nil, stats, err
		}
		if backoff == 0 && policy.Backoff != nil {
			backoff = policy.Backoff(*counter)
		}
		// Aggregate ceiling: the per-class counts bound each failure kind alone,
		// and this bounds their sum. Give up rather than truncate the sleep, so a
		// retry never lands early against a provider still throttling us.
		if sp.MaxTotalWait > 0 && stats.Waited+backoff > sp.MaxTotalWait {
			if logger != nil {
				logger.Warn("LLM call giving up on total backoff ceiling",
					"code", lerr.Code,
					"waited", stats.Waited,
					"next_backoff", backoff,
					"max_total_wait", sp.MaxTotalWait,
					"attempts", stats.Attempts,
					"error", lerr.Message)
			}
			return nil, stats, err
		}
		if logger != nil {
			logger.Warn("LLM call retry",
				"code", lerr.Code,
				"attempt", *counter,
				"max_attempts", policy.MaxAttempts,
				"backoff", backoff,
				"error", lerr.Message)
		}
		select {
		case <-ctx.Done():
			return nil, stats, ctx.Err()
		case <-time.After(backoff):
			stats.Waited += backoff
		}
	}
}

func expBackoff(base time.Duration, factor int, cap time.Duration) func(int) time.Duration {
	if factor < 2 {
		factor = 2
	}
	return func(attempt int) time.Duration {
		d := base
		for i := 1; i < attempt; i++ {
			d = time.Duration(int64(d) * int64(factor))
			if d >= cap {
				d = cap
				break
			}
		}
		// Shave up to a quarter off so callers backing off together do not
		// retry in lockstep. Never exceeds the cap.
		return time.Duration(float64(d) * (0.75 + 0.25*rand.Float64()))
	}
}

// IsFatalLLMError reports whether an LLM error is fatal FOR THE PAYLOAD: sending
// the same request again - here or anywhere - fails the same way. Deliberately
// NARROWER than callOnProvider's fatal set: CodeTruncated and CodeSchemaDrift
// are fatal only on the CURRENT provider, and keeping them out of this list is
// what lets CallLLM hand them to the fallback hook. Do not "unify" the two
// lists - see engine/provider.go for the other side of the split.
func IsFatalLLMError(err error) bool {
	if err == nil {
		return false
	}
	lerr, ok := llm.AsLLMError(err)
	if !ok {
		return false
	}
	switch lerr.Code {
	case llm.CodeAuth, llm.CodeBadRequest, llm.CodeContextOverflow, llm.CodeSchemaUnsupported, llm.CodeQuotaExceeded:
		return true
	}
	return false
}

// errSchemaUnsupported is returned by stage dispatch when the configured
// provider cannot honour the structured-output requirement. Surfaced as
// a typed sentinel so callers can present a clear admin-facing message
// without re-classifying.
var errSchemaUnsupported = errors.New("provider does not support JSON-schema structured output")

// SchemaUnsupportedError wraps the provider name into the sentinel so the
// caller can log which provider was at fault.
func SchemaUnsupportedError(providerType, model string) error {
	return fmt.Errorf("%w: provider=%s model=%s", errSchemaUnsupported, providerType, model)
}

// IsSchemaUnsupported reports whether an error originates from the
// structured-output capability gate.
func IsSchemaUnsupported(err error) bool { return errors.Is(err, errSchemaUnsupported) }
