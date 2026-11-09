//go:build !windows

package winclose

// NotifyOnClose is a no-op outside Windows. SIGINT/SIGTERM via signal.Notify
// cover the equivalent cases on Unix.
func NotifyOnClose(cancel func()) error { return nil }
