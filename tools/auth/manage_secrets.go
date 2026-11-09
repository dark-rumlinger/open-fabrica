package auth

import (
	"fmt"
	"time"

	"openfabrica/events"
	"openfabrica/tools"
)

// manage_secrets is the encrypted at-rest secrets store referenced by
// providers, payments, email, OAuth, and other tools. Other tools accept
// secret_name (not value) so credential material never reaches the LLM
// catalogue or tool-call args.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_secrets",
		Category: "auth",
		OneLiner: "Manage the encrypted secrets store (referenced by providers, payments, email, OAuth).",
		WhenToUse: `Store API keys and signing secrets here once. Tools that take a secret_name
read the value from here at runtime. store itself takes the raw value as an
argument, so for a key the owner holds, ask with manage_chat ask
question_type=secret secret_name=NAME instead: the owner types it in the admin UI
and it is encrypted straight into this store, never entering the conversation.
manage_oauth is the exception: it takes the client secret directly and encrypts
it itself.`,
		Actions: []tools.Action{
			&SecretStore{},
			&SecretList{},
			&SecretDelete{},
		},
	})
}

// SecretStore encrypts the value and persists it under name. Overwrites
// on conflict.
type SecretStore struct {
	Name  string `arg:"name,required"  desc:"Secret name (referenced by other tools)."`
	Value string `arg:"value,required" desc:"Secret value to encrypt and store."`
}

func (a *SecretStore) ActionName() string { return "store" }
func (a *SecretStore) Doc() string        { return "Encrypt and store a named secret." }
func (a *SecretStore) Example() any {
	return &SecretStore{Name: "STRIPE_KEY", Value: "sk_test_..."}
}
func (a *SecretStore) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if ctx.Encryptor == nil {
		return tools.Fail("encryption not available"), nil
	}
	encrypted, err := ctx.Encryptor.Encrypt(a.Value)
	if err != nil {
		return nil, fmt.Errorf("encrypting secret: %w", err)
	}
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_secrets (name, value_encrypted, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
		a.Name, encrypted,
	); err != nil {
		return nil, fmt.Errorf("storing secret: %w", err)
	}
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventSecretStored, ctx.ProjectID, map[string]any{
			"name": a.Name,
		}))
	}
	return tools.Ok(map[string]any{
		"name":    a.Name,
		"message": fmt.Sprintf("Secret '%s' stored successfully", a.Name),
	}), nil
}

// SecretList lists secret names. Values stay encrypted and are never
// returned; the LLM gets enough metadata to reference them by name.
type SecretList struct{}

func (a *SecretList) Doc() string {
	return "List secret names (values stay encrypted, never returned)."
}
func (a *SecretList) ReadOnly() bool { return true }
func (a *SecretList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT id, name, created_at, updated_at FROM of_secrets ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}
	defer rows.Close()
	var secrets []map[string]any
	for rows.Next() {
		var id int
		var name string
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &name, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scanning secret: %w", err)
		}
		secrets = append(secrets, map[string]any{
			"id": id, "name": name,
			"created_at": createdAt, "updated_at": updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}
	return tools.Ok(secrets), nil
}

// SecretDelete removes a stored secret. Tools that reference this name
// must be updated separately.
type SecretDelete struct {
	Name string `arg:"name,required" desc:"Secret name to delete."`
}

func (a *SecretDelete) Doc() string  { return "Delete a stored secret." }
func (a *SecretDelete) Example() any { return &SecretDelete{Name: "STRIPE_KEY"} }
func (a *SecretDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	result, err := ctx.DB.Exec("DELETE FROM of_secrets WHERE name = ?", a.Name)
	if err != nil {
		return nil, fmt.Errorf("deleting secret: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return tools.Fail("secret '%s' not found", a.Name), nil
	}
	tools.LogDestructiveAction(ctx.DB, "manage_secrets", "delete", a.Name)
	return tools.Ok(map[string]any{
		"name":    a.Name,
		"message": fmt.Sprintf("Secret '%s' deleted", a.Name),
	}), nil
}
