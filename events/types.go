package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// WSBroadcaster is implemented by the WebSocket hub to allow server-side
// components (e.g. the Actions Runner) to push messages to connected clients.
type WSBroadcaster interface {
	// BroadcastToRoom sends a JSON message to all clients in the specified room.
	// roomKey MUST be built with RoomKey(projectID, tenantID, endpointPath, room):
	// it is tenant-scoped ("projectID:tenantID:endpointPath:room").
	BroadcastToRoom(roomKey string, msg json.RawMessage)
}

// RoomKey builds the canonical hub room key. It is tenant-scoped: the tenantID
// segment isolates rooms so a client (or a server-side broadcast) in one tenant
// can never address another tenant's room. Every producer of a room key - the WS
// handler and the Actions Runner's BroadcastToRoom - MUST build it here so the
// format lives in exactly one place.
func RoomKey(projectID, tenantID int, endpointPath, room string) string {
	return fmt.Sprintf("%d:%d:%s:%s", projectID, tenantID, endpointPath, room)
}

// RoomKeyPrefix is RoomKey up to and including the trailing ':' before the room
// name - used to enumerate every room of one endpoint within a tenant.
func RoomKeyPrefix(projectID, tenantID int, endpointPath string) string {
	return fmt.Sprintf("%d:%d:%s:", projectID, tenantID, endpointPath)
}

type EventType string

const (
	EventProjectCreated   EventType = "project.created"
	EventProjectUpdated   EventType = "project.updated"
	EventProjectDeleted   EventType = "project.deleted"
	EventToolExecuted     EventType = "tool.executed"
	EventToolFailed       EventType = "tool.failed"
	EventChatMessage      EventType = "chat.message"
	EventProviderAdded    EventType = "provider.added"
	EventProviderUpdated  EventType = "provider.updated"
	EventQuestionAsked    EventType = "question.asked"
	EventQuestionAnswered EventType = "question.answered"
	// Engine events: the build engine's lifecycle + progress stream that the
	// admin UI and bot subscribe to for live build feedback.
	EventEngineStarted     EventType = "engine.started"
	EventEngineStopped     EventType = "engine.stopped"
	EventEngineError       EventType = "engine.error"
	EventEngineMessage     EventType = "engine.message"
	EventEngineToolStart   EventType = "engine.tool_start"
	EventEngineToolResult  EventType = "engine.tool_result"
	EventEngineStageChange EventType = "engine.stage_change"
	EventEngineThinking    EventType = "engine.thinking"
	EventEngineTasks       EventType = "engine.tasks"
	EventEngineStopLanded  EventType = "engine.stop_landed"
	// The owner-facing phase of a project changed (see internal/projectphase).
	// Payload carries the stored mode and the derived phase.
	EventProjectPhaseChanged EventType = "project.phase_changed"
	// One LLM round-trip finished and was logged (model, tokens, timing, cost).
	EventEngineLLMCall      EventType = "engine.llm_call"
	EventWebhookReceived    EventType = "webhook.received"
	EventSecretStored       EventType = "secret.stored"
	EventDataInsert         EventType = "data.insert"
	EventDataUpdate         EventType = "data.update"
	EventDataDelete         EventType = "data.delete"
	EventAuthRegister       EventType = "auth.register"
	EventAuthLogin          EventType = "auth.login"
	EventPaymentCompleted   EventType = "payment.completed"
	EventPaymentFailed      EventType = "payment.failed"
	EventSettingsChanged    EventType = "settings.changed"
	EventSchemaCreated      EventType = "schema.created"
	EventSchemaAltered      EventType = "schema.altered"
	EventScheduledCompleted EventType = "scheduled.completed"
	EventScheduledFailed    EventType = "scheduled.failed"
	// Internal (not project-facing): an authoritative endpoint's reducer was attached or
	// replaced (manage_authoritative set_reducer). The public server hands every live
	// room of that endpoint the new code instead of waiting for the next join.
	EventWSReducerChanged EventType = "ws.reducer_changed"
	// Multi-tenancy lifecycle. Projects with manage_tenancy enabled fire these
	// so manage_triggers can wire welcome emails, audit logs, or third-party
	// CRM sync without the tool itself needing to grow new outputs.
	EventTenantCreated       EventType = "tenant.created"
	EventTenantMemberInvited EventType = "tenant.member_invited"
	EventTenantMemberJoined  EventType = "tenant.member_joined"
	EventTenantSwitched      EventType = "tenant.switched"
	// In-app notification created via manage_notifications - a project-facing
	// trigger event (documented in the trigger-events contract).
	EventNotificationCreated EventType = "notification.created"
	// Internal (not project-facing): a person finished pairing their messaging
	// account with the bot from an invite link. The admin setup page listens for
	// it to flip from "waiting" to "connected".
	EventBotPaired EventType = "bot.paired"
)

type Event struct {
	// ID is a monotonically increasing event sequence assigned by the Bus on
	// Publish. Zero for events that haven't been published (test fixtures, etc).
	ID        uint64         `json:"event_id"`
	Type      EventType      `json:"type"`
	ProjectID int            `json:"project_id,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

func NewEvent(eventType EventType, projectID int, payload map[string]any) Event {
	return Event{
		Type:      eventType,
		ProjectID: projectID,
		Payload:   payload,
		Timestamp: time.Now(),
	}
}
