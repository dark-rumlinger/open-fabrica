package llm

import (
	"context"
	"errors"
	"sync"
	"time"

	"openfabrica/internal/stringutil"
)

// ProviderHealth is what the last real calls to one provider say about it. It is
// kept in memory (a restart starts clean, which is the honest answer: nothing has
// been tried since), and records calls the app makes anyway, so a provider that
// stopped working shows up without anyone pressing Test.
type ProviderHealth struct {
	LastOK       time.Time `json:"last_ok,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrCode  string    `json:"last_error_code,omitempty"`
	Consecutive  int       `json:"consecutive_failures"`
	TotalOK      int       `json:"total_ok"`
	TotalFailure int       `json:"total_failures"`
}

// maxHealthError bounds the stored message; provider bodies can be large.
const maxHealthError = 300

var (
	healthMu sync.Mutex
	health   = map[string]*ProviderHealth{}
)

// RecordProviderCall notes the outcome of one call to the named provider. A
// cancelled context is the caller giving up, not the provider failing, so it is
// ignored.
func RecordProviderCall(name string, err error) {
	if name == "" {
		return
	}
	if err != nil && errors.Is(err, context.Canceled) {
		return
	}
	healthMu.Lock()
	defer healthMu.Unlock()
	h := health[name]
	if h == nil {
		h = &ProviderHealth{}
		health[name] = h
	}
	now := time.Now()
	if err == nil {
		h.LastOK = now
		h.Consecutive = 0
		h.TotalOK++
		return
	}
	msg := stringutil.Truncate(err.Error(), maxHealthError)
	h.LastErrorAt = now
	h.LastError = msg
	h.LastErrCode = ""
	if lerr, ok := AsLLMError(err); ok {
		h.LastErrCode = lerr.Code
	}
	h.Consecutive++
	h.TotalFailure++
}

// HealthOf returns the recorded health of a provider, and false when nothing has
// been recorded for it since the process started.
func HealthOf(name string) (ProviderHealth, bool) {
	healthMu.Lock()
	defer healthMu.Unlock()
	h, ok := health[name]
	if !ok {
		return ProviderHealth{}, false
	}
	return *h, true
}

// ResetHealth clears the record (tests, and a provider's key being replaced).
func ResetHealth(name string) {
	healthMu.Lock()
	defer healthMu.Unlock()
	if name == "" {
		clear(health)
		return
	}
	delete(health, name)
}
