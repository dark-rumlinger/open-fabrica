package security

import "testing"

func TestValidateColumnName(t *testing.T) {
	good := []string{"id", "user_id", "userId", "a", "A1", "col_2_x"}
	for _, n := range good {
		if err := ValidateColumnName(n); err != nil {
			t.Errorf("ValidateColumnName(%q) unexpected error: %v", n, err)
		}
	}
	bad := []string{
		"",
		"1col",
		"col-name",
		"col name",
		"col;DROP TABLE x",
		"col\"",
		"col'",
		"col;",
		"naïve",
		"col.name",
		"\"col\"",
	}
	for _, n := range bad {
		if err := ValidateColumnName(n); err == nil {
			t.Errorf("ValidateColumnName(%q) expected error, got nil", n)
		}
	}
}

func TestValidateColumnName_ReservedWords(t *testing.T) {
	reserved := []string{"order", "ORDER", "Order", "group", "select", "from", "where", "index", "key", "references"}
	for _, n := range reserved {
		err := ValidateColumnName(n)
		if err == nil {
			t.Errorf("ValidateColumnName(%q) expected reserved-word error, got nil", n)
			continue
		}
		if !contains(err.Error(), "reserved word") {
			t.Errorf("ValidateColumnName(%q) error %q should mention 'reserved word'", n, err.Error())
		}
	}
	// Suggestion enrichment: "order" should suggest sort_order.
	if err := ValidateColumnName("order"); err == nil || !contains(err.Error(), "sort_order") {
		t.Errorf("ValidateColumnName(\"order\") should suggest sort_order, got %v", err)
	}
	// Non-reserved names that happen to look keyword-ish must still pass.
	ok := []string{"name", "type", "value", "status", "data", "text", "title", "sort_order", "group_name"}
	for _, n := range ok {
		if err := ValidateColumnName(n); err != nil {
			t.Errorf("ValidateColumnName(%q) unexpected error: %v", n, err)
		}
	}
}

func TestValidateTableName_ReservedWords(t *testing.T) {
	if err := ValidateTableName("order"); err == nil {
		t.Errorf("ValidateTableName(\"order\") expected error, got nil")
	}
	if err := ValidateTableName("orders"); err != nil {
		t.Errorf("ValidateTableName(\"orders\") unexpected error: %v", err)
	}
}

func TestIsSQLiteReservedWord(t *testing.T) {
	if !IsSQLiteReservedWord("order") || !IsSQLiteReservedWord("ORDER") {
		t.Fatal("expected order/ORDER to be reserved")
	}
	if IsSQLiteReservedWord("orders") || IsSQLiteReservedWord("sort_order") {
		t.Fatal("expected orders/sort_order to not be reserved")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestValidateFKReference(t *testing.T) {
	good := []string{"users(id)", "auth_users(user_id)", "T(c)"}
	for _, n := range good {
		if err := ValidateFKReference(n); err != nil {
			t.Errorf("ValidateFKReference(%q) unexpected error: %v", n, err)
		}
	}
	bad := []string{
		"",
		"users",
		"users()",
		"users(id",
		"users id)",
		"1users(id)",
		"users(1id)",
		"users(id);DROP",
		"users(id) extra",
	}
	for _, n := range bad {
		if err := ValidateFKReference(n); err == nil {
			t.Errorf("ValidateFKReference(%q) expected error, got nil", n)
		}
	}
}

func TestOwnerFilterClause(t *testing.T) {
	clause, err := OwnerFilterClause("")
	if err != nil || clause != "" {
		t.Fatalf("empty column: got (%q,%v), want (\"\",nil)", clause, err)
	}

	clause, err = OwnerFilterClause("user_id")
	if err != nil {
		t.Fatalf("user_id: unexpected error %v", err)
	}
	if clause != `"user_id" = ?` {
		t.Fatalf("user_id: got %q, want %q", clause, `"user_id" = ?`)
	}

	if _, err := OwnerFilterClause("user_id; DROP TABLE x"); err == nil {
		t.Fatalf("malicious column: expected error, got nil")
	}
	if _, err := OwnerFilterClause("1bad"); err == nil {
		t.Fatalf("leading digit: expected error, got nil")
	}
}
