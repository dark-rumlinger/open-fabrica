package bot

import (
	"sync"
	"sync/atomic"
	"time"
)

// Process-wide bot counters, surfaced on the admin system-status endpoint so a
// silently-misbehaving bot is visible (mirrors engine.DroppedBackgroundCommands).
var (
	botMessagesIn   atomic.Uint64
	botMessagesOut  atomic.Uint64
	botSendFailures atomic.Uint64
	botAuthDenials  atomic.Uint64
	botBackoffNanos atomic.Int64 // current poll backoff; 0 when healthy
	botLastPollUnix atomic.Int64 // last successful getUpdates (unix seconds)

	botHealthMu     sync.Mutex
	botHealthy      bool
	botHealthDetail string
)

func incMessagesIn()   { botMessagesIn.Add(1) }
func incMessagesOut()  { botMessagesOut.Add(1) }
func incSendFailures() { botSendFailures.Add(1) }
func incAuthDenials()  { botAuthDenials.Add(1) }

func setBotBackoff(d time.Duration) { botBackoffNanos.Store(int64(d)) }

func markBotPoll() {
	botLastPollUnix.Store(time.Now().Unix())
	setBotHealth(true, "")
}

func setBotHealth(ok bool, detail string) {
	botHealthMu.Lock()
	botHealthy = ok
	botHealthDetail = detail
	botHealthMu.Unlock()
}

// resetBotStats clears health once the bot is disconnected, so the admin page
// does not keep showing the last token's state.
func resetBotStats() {
	setBotHealth(false, "")
	botLastPollUnix.Store(0)
	botBackoffNanos.Store(0)
}

// Stats is a point-in-time snapshot of bot health for the admin UI.
type Stats struct {
	MessagesIn    uint64 `json:"messages_in"`
	MessagesOut   uint64 `json:"messages_out"`
	SendFailures  uint64 `json:"send_failures"`
	AuthDenials   uint64 `json:"auth_denials"`
	BackoffActive bool   `json:"backoff_active"`
	Healthy       bool   `json:"healthy"`
	HealthDetail  string `json:"health_detail,omitempty"`
	LastPollUnix  int64  `json:"last_poll_unix"`
}

// GetStats returns a snapshot of the process-wide bot counters.
func GetStats() Stats {
	botHealthMu.Lock()
	healthy := botHealthy
	detail := botHealthDetail
	botHealthMu.Unlock()
	return Stats{
		MessagesIn:    botMessagesIn.Load(),
		MessagesOut:   botMessagesOut.Load(),
		SendFailures:  botSendFailures.Load(),
		AuthDenials:   botAuthDenials.Load(),
		BackoffActive: botBackoffNanos.Load() > 0,
		Healthy:       healthy,
		HealthDetail:  detail,
		LastPollUnix:  botLastPollUnix.Load(),
	}
}
