package bot

import (
	"strings"

	"openfabrica/db"
	"openfabrica/security"
)

const (
	// TokenSettingKey is the settings row holding the Telegram bot token.
	TokenSettingKey = "telegram_bot_token"
	// TokenMask is what the admin API returns in place of the stored token, and
	// what a client sends back to mean "leave the token unchanged".
	TokenMask = "********"

	tokenEncPrefix = "enc:v1:"
)

// LoadBotToken returns the plaintext bot token. Values stored before encryption
// was introduced are returned as-is, so upgrades need no migration step.
func LoadBotToken(database *db.DB, enc *security.Encryptor) string {
	raw, err := database.GetSetting(TokenSettingKey)
	if err != nil {
		return ""
	}
	return decodeBotToken(raw, enc)
}

func decodeBotToken(raw string, enc *security.Encryptor) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, tokenEncPrefix) {
		return raw
	}
	if enc == nil {
		return ""
	}
	plain, err := enc.Decrypt(strings.TrimPrefix(raw, tokenEncPrefix))
	if err != nil {
		return ""
	}
	return plain
}

// EncodeBotToken prepares a token for storage: encrypted when an encryptor is
// available, plaintext otherwise (tests, minimal setups). An empty token stays
// empty so clearing the field clears the setting.
func EncodeBotToken(token string, enc *security.Encryptor) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" || enc == nil {
		return token, nil
	}
	sealed, err := enc.Encrypt(token)
	if err != nil {
		return "", err
	}
	return tokenEncPrefix + sealed, nil
}

// SaveBotToken stores the token encrypted at rest.
func SaveBotToken(database *db.DB, enc *security.Encryptor, token string) error {
	stored, err := EncodeBotToken(token, enc)
	if err != nil {
		return err
	}
	return database.SetSetting(TokenSettingKey, stored)
}

// UpgradeBotToken re-saves a legacy plaintext token in encrypted form.
func UpgradeBotToken(database *db.DB, enc *security.Encryptor) {
	if enc == nil {
		return
	}
	raw, err := database.GetSetting(TokenSettingKey)
	raw = strings.TrimSpace(raw)
	if err != nil || raw == "" || strings.HasPrefix(raw, tokenEncPrefix) {
		return
	}
	_ = SaveBotToken(database, enc, raw)
}
