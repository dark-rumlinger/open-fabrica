package triggers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/safego"
)

const auditQueueCapacity = 256

// AuditLogger subscribes to key events and logs them to of_activity_log.
type AuditLogger struct {
	projectDBMgr *db.ProjectDBManager
	logger       *slog.Logger
	queue        chan events.Event
	mu           sync.RWMutex
	closed       bool
	stopOnce     sync.Once
	done         chan struct{}
}

// NewAuditLogger creates an audit logger and subscribes to relevant events.
func NewAuditLogger(projectDBMgr *db.ProjectDBManager, bus *events.Bus) *AuditLogger {
	al := &AuditLogger{
		projectDBMgr: projectDBMgr,
		logger:       slog.With("component", "audit_logger"),
		queue:        make(chan events.Event, auditQueueCapacity),
		done:         make(chan struct{}),
	}

	safego.Go("audit-logger", al.run)

	// Subscribe to security and data-relevant events.
	for _, et := range []events.EventType{
		events.EventAuthRegister,
		events.EventAuthLogin,
		events.EventPaymentCompleted,
		events.EventPaymentFailed,
		events.EventDataInsert,
		events.EventDataUpdate,
		events.EventDataDelete,
		events.EventSchemaCreated,
		events.EventSchemaAltered,
		events.EventSecretStored,
		events.EventSettingsChanged,
	} {
		bus.Subscribe(et, al.handleEvent)
	}

	return al
}

func (al *AuditLogger) handleEvent(event events.Event) {
	if event.ProjectID == 0 {
		return
	}
	al.mu.RLock()
	defer al.mu.RUnlock()
	if al.closed {
		return
	}
	select {
	case al.queue <- event:
	default:
		al.logger.Debug("audit log queue full, dropping event", "event", event.Type, "project_id", event.ProjectID)
	}
}

// Stop closes the audit queue and waits for buffered events to drain.
func (al *AuditLogger) Stop() {
	al.stopOnce.Do(func() {
		al.mu.Lock()
		al.closed = true
		close(al.queue)
		al.mu.Unlock()
		<-al.done
	})
}

func (al *AuditLogger) run() {
	defer close(al.done)
	for event := range al.queue {
		projectDB := al.projectDBMgr.Get(event.ProjectID)
		if projectDB == nil {
			continue
		}

		summary := buildAuditSummary(event)
		details, _ := json.Marshal(event.Payload)

		_, err := projectDB.ExecWrite(
			"INSERT INTO of_activity_log (event_type, summary, details) VALUES (?, ?, ?)",
			string(event.Type), summary, string(details),
		)
		if err != nil {
			al.logger.Debug("audit log write failed", "event", event.Type, "error", err)
		}
	}
}

// buildAuditSummary creates a human-readable summary for common event types.
func buildAuditSummary(event events.Event) string {
	p := event.Payload

	switch event.Type {
	case events.EventAuthRegister:
		return fmt.Sprintf("New user registered in %v", p["table"])
	case events.EventAuthLogin:
		return fmt.Sprintf("User logged in (table: %v)", p["table"])
	case events.EventPaymentCompleted:
		return fmt.Sprintf("Payment completed: %v", p["external_id"])
	case events.EventPaymentFailed:
		return fmt.Sprintf("Payment failed: %v", p["external_id"])
	case events.EventDataInsert:
		return fmt.Sprintf("Row inserted in %v", p["table"])
	case events.EventDataUpdate:
		return fmt.Sprintf("Row updated in %v (id: %v)", p["table"], p["id"])
	case events.EventDataDelete:
		return fmt.Sprintf("Row deleted from %v (id: %v)", p["table"], p["id"])
	case events.EventSchemaCreated:
		return fmt.Sprintf("Table created: %v", p["table"])
	case events.EventSchemaAltered:
		return fmt.Sprintf("Table altered: %v", p["table"])
	case events.EventSecretStored:
		return fmt.Sprintf("Secret stored: %v", p["name"])
	case events.EventSettingsChanged:
		return "Settings changed"
	default:
		return string(event.Type)
	}
}
