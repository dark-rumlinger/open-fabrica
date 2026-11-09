package store

import "testing"

// Every of_* system table must be reserved. The execute_sql tool blocks SELECTs
// against system tables by this list, and the admin add-column path refuses
// them by it too, so a gap exposes auth tokens, TOTP secrets, push endpoints,
// tenancy and subscription rows to a SELECT-only data query.
func TestSystemTablesIncludeSensitiveTables(t *testing.T) {
	mustReserve := []string{
		"of_magic_tokens", "of_user_totp", "of_totp_endpoints",
		"of_magic_link_endpoints", "of_push_subscriptions", "of_push_endpoints",
		"of_tenants", "of_tenant_members", "of_tenant_invites",
		"of_subscriptions", "of_subscription_plans",
		"of_payments", "of_payment_config",
		"of_symbols", "of_symbol_refs", "of_symbol_index_state",
		"of_export_endpoints", "of_import_endpoints", "of_feed_endpoints",
		"of_page_assets", "of_llm_usage_daily",
	}
	for _, name := range mustReserve {
		if !IsSystemTable(name) {
			t.Errorf("%s must be a reserved system table (execute_sql leak guard / DDL guard)", name)
		}
	}
}

// The reserved set is the whole of_ namespace, not an enumerated list: any
// of_-prefixed name is reserved (so a newly added system table is protected
// without editing a registry), while LLM/user tables outside the namespace are
// not. SanitizedTableName must refuse the of_ prefix on create.
func TestSystemTableNamespace(t *testing.T) {
	if !IsSystemTable("of_some_future_table") {
		t.Error("any of_ table must be reserved, even ones not yet defined")
	}
	for _, userTable := range []string{"posts", "users", "number_of_items", "ofx_data"} {
		if IsSystemTable(userTable) {
			t.Errorf("%s is outside the of_ namespace and must not be reserved", userTable)
		}
	}
	if _, err := SanitizedTableName("of_posts"); err == nil {
		t.Error("SanitizedTableName must reject the reserved of_ prefix")
	}
	if _, err := SanitizedTableName("posts"); err != nil {
		t.Errorf("SanitizedTableName rejected a valid user table: %v", err)
	}

	// migrations lives outside the of_ namespace (the runner creates it before
	// any migration runs) but must still be reserved so an LLM-created table
	// cannot collide with the migration tracker.
	if !IsSystemTable("migrations") {
		t.Error("migrations must be reserved (collision with the migration tracker)")
	}
	if _, err := SanitizedTableName("migrations"); err == nil {
		t.Error("SanitizedTableName must reject the migrations tracker name")
	}
}
