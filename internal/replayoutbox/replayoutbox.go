// Package replayoutbox keeps the messages an app sends while one of its user-flow
// replays (of_test.js) runs. A replay signs up, orders, resets passwords and fills
// in contact forms like a person does, and each of those can send an email
// or a push notification. Delivered, they would reach the project owner (a contact
// form's "new message" mail) or real subscribers on every replay; kept here instead,
// a flow can open the confirmation link, read the one-time code or check the owner
// was notified - which is how a person would verify those journeys.
//
// Capture happens in the send actions themselves, after they have checked the
// provider is configured, so an app that could not actually send still fails the
// same way during a replay.
package replayoutbox

import (
	"sync"
	"time"
)

// Message is one captured email or push notification.
type Message struct {
	Kind    string // email | push
	To      string // address, phone number, or push endpoint (and user)
	Subject string // email subject or push title
	Body    string
	At      time.Time
}

// maxMessages bounds one replay's outbox (a loop that mails every tick).
const maxMessages = 200

type box struct {
	depth int
	msgs  []Message
}

var (
	mu    sync.Mutex
	boxes = map[int]*box{}
)

// Begin starts capturing project projectID's outgoing messages and returns the
// func that stops it. Calls nest: capture lasts until every Begin has ended, and
// the messages are dropped when the last one does.
func Begin(projectID int) (end func()) {
	mu.Lock()
	b := boxes[projectID]
	if b == nil {
		b = &box{}
		boxes[projectID] = b
	}
	b.depth++
	mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			if b.depth--; b.depth <= 0 {
				delete(boxes, projectID)
			}
		})
	}
}

// Active reports whether a replay of the project is running.
func Active(projectID int) bool {
	mu.Lock()
	defer mu.Unlock()
	return boxes[projectID] != nil
}

// Capture keeps m when a replay of the project is running and reports whether it
// did; the caller delivers the message only when it did not.
func Capture(projectID int, m Message) bool {
	mu.Lock()
	defer mu.Unlock()
	b := boxes[projectID]
	if b == nil {
		return false
	}
	if m.At.IsZero() {
		m.At = time.Now()
	}
	if len(b.msgs) < maxMessages {
		b.msgs = append(b.msgs, m)
	}
	return true
}

// Messages returns a copy of the project's captured messages, oldest first.
func Messages(projectID int) []Message {
	mu.Lock()
	defer mu.Unlock()
	b := boxes[projectID]
	if b == nil {
		return nil
	}
	return append([]Message(nil), b.msgs...)
}
