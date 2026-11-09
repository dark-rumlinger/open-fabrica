package bot

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	"openfabrica/db"
)

// LoadSessions restores bot user sessions from the database.
func LoadSessions(database *db.DB) map[string]*UserState {
	sessions := make(map[string]*UserState)
	rows, err := database.DB.Query(`
		SELECT provider, chat_id, user_id, active_project_id, muted,
		       pending_action, pending_action_at, pending_data,
		       last_status_message_id, last_status_at, last_status_text,
		       verbosity, live_card_message_id
		FROM bot_sessions`)
	if err != nil {
		return sessions
	}
	defer rows.Close()

	for rows.Next() {
		var (
			provider, chatID, userID   string
			activeProjectID            int
			muted                      bool
			pendingAction, pendingData string
			pendingActionAt            sql.NullTime
			lastStatusMessageID        int
			lastStatusAt               sql.NullTime
			lastStatusText             string
			verbosity                  string
			liveCardMessageID          int
		)
		if err := rows.Scan(
			&provider, &chatID, &userID, &activeProjectID, &muted,
			&pendingAction, &pendingActionAt, &pendingData,
			&lastStatusMessageID, &lastStatusAt, &lastStatusText,
			&verbosity, &liveCardMessageID,
		); err != nil {
			slog.Warn("bot.LoadSessions: skipping unreadable session row", "error", err)
			continue
		}
		var pdata map[string]string
		if pendingData != "" {
			if err := json.Unmarshal([]byte(pendingData), &pdata); err != nil {
				slog.Warn("bot.LoadSessions: malformed pending_data, dropping", "provider", provider, "chat_id", chatID, "error", err)
				pdata = nil
			}
		}
		key := provider + ":" + chatID
		sessions[key] = &UserState{
			Provider:            provider,
			ChatID:              chatID,
			UserID:              userID,
			ActiveProjectID:     activeProjectID,
			Muted:               muted,
			PendingAction:       pendingAction,
			PendingActionAt:     nullTime(pendingActionAt),
			PendingData:         pdata,
			LastStatusMessageID: lastStatusMessageID,
			LastStatusAt:        nullTime(lastStatusAt),
			LastStatusText:      lastStatusText,
			LiveCardMessageID:   liveCardMessageID,
		}
		sessions[key].setVerbosity(verbosity)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("bot.LoadSessions: session read failed", "error", err)
	}
	return sessions
}

// SaveSession persists a user's session state.
func SaveSession(database *db.DB, state *UserState) {
	var pendingData string
	if len(state.PendingData) > 0 {
		if b, err := json.Marshal(state.PendingData); err == nil {
			pendingData = string(b)
		}
	}
	if _, err := database.ExecWrite(
		`INSERT INTO bot_sessions (
			provider, chat_id, user_id, active_project_id, muted,
			pending_action, pending_action_at, pending_data,
			last_status_message_id, last_status_at, last_status_text,
			verbosity, live_card_message_id,
			updated_at
		)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(provider, chat_id) DO UPDATE SET
		   user_id = excluded.user_id,
		   active_project_id = excluded.active_project_id,
		   muted = excluded.muted,
		   pending_action = excluded.pending_action,
		   pending_action_at = excluded.pending_action_at,
		   pending_data = excluded.pending_data,
		   last_status_message_id = excluded.last_status_message_id,
		   last_status_at = excluded.last_status_at,
		   last_status_text = excluded.last_status_text,
		   verbosity = excluded.verbosity,
		   live_card_message_id = excluded.live_card_message_id,
		   updated_at = CURRENT_TIMESTAMP`,
		state.Provider, state.ChatID, state.UserID, state.ActiveProjectID, state.Muted,
		state.PendingAction, nullableTime(state.PendingActionAt), pendingData,
		state.LastStatusMessageID, nullableTime(state.LastStatusAt), state.LastStatusText,
		state.verbosity(), state.LiveCardMessageID,
	); err != nil {
		slog.Warn("bot.SaveSession: persist failed", "provider", state.Provider, "chat_id", state.ChatID, "error", err)
	}
}

func nullTime(nt sql.NullTime) time.Time {
	if nt.Valid {
		return nt.Time
	}
	return time.Time{}
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
