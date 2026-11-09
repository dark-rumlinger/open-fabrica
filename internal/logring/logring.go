// Package logring keeps the most recent log lines in memory so the admin panel can
// show them without anyone opening a terminal. It is an io.Writer, meant to sit
// beside stdout in the logger's output.
package logring

import (
	"strings"
	"sync"
)

// DefaultLines is how many lines the process-wide ring holds.
const DefaultLines = 2000

// Default is the ring the server logs into.
var Default = New(DefaultLines)

// Ring is a fixed-size buffer of the latest lines.
type Ring struct {
	mu      sync.Mutex
	lines   []string
	next    int
	full    bool
	partial strings.Builder // the unfinished line a Write left behind
}

// New returns a ring holding up to n lines.
func New(n int) *Ring {
	if n < 1 {
		n = 1
	}
	return &Ring{lines: make([]string, n)}
}

// Write splits p into lines and stores them. It never fails: a log writer that
// errors would take the logger down with it.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := string(p)
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			r.partial.WriteString(text)
			break
		}
		r.partial.WriteString(text[:i])
		r.push(strings.TrimRight(r.partial.String(), "\r"))
		r.partial.Reset()
		text = text[i+1:]
	}
	return len(p), nil
}

func (r *Ring) push(line string) {
	r.lines[r.next] = line
	r.next++
	if r.next == len(r.lines) {
		r.next = 0
		r.full = true
	}
}

// Last returns up to n of the newest lines, oldest first.
func (r *Ring) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := r.next
	if r.full {
		count = len(r.lines)
	}
	if n <= 0 || n > count {
		n = count
	}
	out := make([]string, 0, n)
	start := r.next - n
	for i := 0; i < n; i++ {
		idx := (start + i) % len(r.lines)
		if idx < 0 {
			idx += len(r.lines)
		}
		out = append(out, r.lines[idx])
	}
	return out
}
