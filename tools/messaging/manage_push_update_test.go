package messaging

import (
	"testing"

	"openfabrica/tools/tooltest"
)

// Before this change contact_email / requires_auth could never be changed after
// create (create early-returns on an existing path, and the INSERT's ON CONFLICT was
// dead code). action=update is the single supported way to change them.
func TestManagePush_UpdateChangesContactAndAuth(t *testing.T) {
	h := tooltest.New(t)
	h.MustOK("manage_push", map[string]any{
		"action":        "create",
		"endpoint_path": "news",
		"contact_email": "first@example.com",
	})

	h.MustOK("manage_push", map[string]any{
		"action":        "update",
		"endpoint_path": "news",
		"contact_email": "second@example.com",
		"requires_auth": true,
	})

	var contact string
	var requiresAuth bool
	if err := h.DB().QueryRow(
		"SELECT contact_email, requires_auth FROM of_push_endpoints WHERE path = ?", "news",
	).Scan(&contact, &requiresAuth); err != nil {
		t.Fatalf("reading push endpoint: %v", err)
	}
	if contact != "second@example.com" {
		t.Errorf("contact_email = %q, want second@example.com", contact)
	}
	if !requiresAuth {
		t.Errorf("requires_auth = false, want true")
	}

	// Omitted fields are left unchanged: toggle auth back off without touching contact.
	h.MustOK("manage_push", map[string]any{
		"action":        "update",
		"endpoint_path": "news",
		"requires_auth": false,
	})
	if err := h.DB().QueryRow(
		"SELECT contact_email, requires_auth FROM of_push_endpoints WHERE path = ?", "news",
	).Scan(&contact, &requiresAuth); err != nil {
		t.Fatalf("reading push endpoint: %v", err)
	}
	if contact != "second@example.com" || requiresAuth {
		t.Errorf("after auth-only update: contact=%q auth=%v, want second@example.com/false", contact, requiresAuth)
	}
}

// Updating a path that was never created is a clear not-found, not a silent no-op.
func TestManagePush_UpdateMissingEndpointFails(t *testing.T) {
	h := tooltest.New(t)
	h.MustFail("manage_push", map[string]any{
		"action":        "update",
		"endpoint_path": "ghost",
		"contact_email": "x@example.com",
	}, "")
}
