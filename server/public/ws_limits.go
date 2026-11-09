package public

import (
	"fmt"
	"sync"
)

const (
	// wsHardReadLimit is the largest inbound frame any socket accepts, whatever
	// max_msg_size says (0 means "no endpoint cap", not "unbounded memory").
	wsHardReadLimit = 8 << 20
	// wsMaxConnsPerVisitor caps concurrent sockets from one visitor on one
	// project. Generous enough for a classroom behind one NAT, small enough that a
	// single client cannot exhaust the goroutines and buffers each socket holds.
	wsMaxConnsPerVisitor = 100
	// wsMaxRoomName bounds a room name taken from the query string.
	wsMaxRoomName = 128
)

var (
	wsConnMu     sync.Mutex
	wsConnCounts = map[string]int{}
)

// acquireWSConn reserves a socket slot for (project, visitor). The returned
// release must be called when the socket closes.
func acquireWSConn(projectID int, visitor string) (release func(), ok bool) {
	key := fmt.Sprintf("%d|%s", projectID, visitor)
	wsConnMu.Lock()
	defer wsConnMu.Unlock()
	if wsConnCounts[key] >= wsMaxConnsPerVisitor {
		return nil, false
	}
	wsConnCounts[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			wsConnMu.Lock()
			defer wsConnMu.Unlock()
			if wsConnCounts[key] <= 1 {
				delete(wsConnCounts, key)
			} else {
				wsConnCounts[key]--
			}
		})
	}, true
}

// wsReadLimit is the frame cap for a socket: the endpoint's max_msg_size plus
// slack for the in-band "too large" reply, never above the hard ceiling.
func wsReadLimit(maxMsgSize int) int64 {
	if maxMsgSize <= 0 {
		return wsHardReadLimit
	}
	limit := int64(maxMsgSize) + 16384
	if limit > wsHardReadLimit {
		return wsHardReadLimit
	}
	return limit
}
