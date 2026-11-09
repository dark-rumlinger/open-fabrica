package admin

import (
	"net/http"
	"os"
	"strings"

	"openfabrica/bot"
	"openfabrica/events"
)

const (
	// budgetEnvVar overrides the saved daily token budget when set.
	budgetEnvVar = "OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET"
	// budgetEnvSettingKey is the derived key Get adds to carry that override's value.
	budgetEnvSettingKey = "engine_daily_token_budget_env"
)

// SettingsHandler handles system settings get/put.
type SettingsHandler struct {
	deps *Deps
}

// Get returns all settings as a key-value map.
func (h *SettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	rows, err := h.deps.DB.Query("SELECT key, value FROM settings ORDER BY key")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		settings[key] = value
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}

	// The bot token never leaves the server; clients get a mask plus a flag.
	if strings.TrimSpace(settings[bot.TokenSettingKey]) != "" {
		settings[bot.TokenSettingKey] = bot.TokenMask
		settings[bot.TokenSettingKey+"_set"] = "1"
	}

	// The token-budget env var outranks the saved setting (see
	// resolveEngineDailyTokenBudget), so tell the page when an edit there is
	// shadowed. Derived like the bot-token flag: never stored.
	if v, ok := os.LookupEnv(budgetEnvVar); ok {
		settings[budgetEnvSettingKey] = v
	}

	writeJSON(w, http.StatusOK, settings)
}

// Update sets one or more settings from a key-value map.
func (h *SettingsHandler) Update(w http.ResponseWriter, r *http.Request) {
	var settings map[string]string
	if !decodeJSON(w, r, &settings) {
		return
	}

	for key, value := range settings {
		switch key {
		case bot.TokenSettingKey:
			if value == bot.TokenMask {
				continue // unchanged
			}
			stored, err := bot.EncodeBotToken(value, h.deps.Encryptor)
			if err != nil {
				h.deps.Logger.Error("failed to encrypt bot token", "error", err)
				writeError(w, http.StatusInternalServerError, "failed to save settings")
				return
			}
			value = stored
		case bot.TokenSettingKey + "_set", budgetEnvSettingKey:
			continue // derived flag, never stored
		}
		if err := h.deps.DB.SetSetting(key, value); err != nil {
			h.deps.Logger.Error("failed to set setting", "key", key, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to save settings")
			return
		}
	}

	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventSettingsChanged, 0, nil))
	}

	writeStatus(w, "updated")
}
