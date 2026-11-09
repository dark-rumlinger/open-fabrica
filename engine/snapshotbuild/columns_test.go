package snapshotbuild

import (
	"reflect"
	"testing"
)

// The schema_def manage_schema writes is an object keyed by column name. Every column
// must reach the snapshot, with its type, and NOT NULL only where the caller must send it.
func TestParseColumnListObjectSchema(t *testing.T) {
	def := `{"customer_name":{"not_null":true,"type":"TEXT"},"drink":"TEXT",` +
		`"status":{"default":"queued","not_null":true,"type":"TEXT"},"takeaway":{"default":false,"type":"BOOLEAN"}}`
	got := parseColumnList(def)
	want := []string{"customer_name TEXT NOT NULL", "drink TEXT", "status TEXT", "takeaway BOOLEAN"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseColumnList = %q, want %q", got, want)
	}
	if got := parseColumnList(""); got != nil {
		t.Fatalf("empty schema = %q, want nil", got)
	}
}
