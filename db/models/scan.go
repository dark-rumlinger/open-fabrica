package models

import (
	"reflect"
	"strings"
)

// Single source of truth for a model's columns: a struct's `db:"col"` tags drive
// both the SELECT list (dbColumns) and the Scan targets (dbScanTargets) in field
// order, so the two cannot drift. Fields without a db tag (or `db:"-"`) are skipped.

// dbColumns returns the comma-separated column list for a struct value or
// pointer, e.g. "id, name, created_at".
func dbColumns(v any) string {
	return strings.Join(dbColumnNames(v), ", ")
}

// dbColumnsAlias is dbColumns with every column prefixed by "<alias>." for
// joined queries where a bare `id` would be ambiguous.
func dbColumnsAlias(v any, alias string) string {
	names := dbColumnNames(v)
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = alias + "." + n
	}
	return strings.Join(out, ", ")
}

func dbColumnNames(v any) []string {
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var names []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("db")
		if tag == "" || tag == "-" {
			continue
		}
		names = append(names, tag)
	}
	return names
}

// dbScanTargets returns &field for each db-tagged field of the struct pointed
// to by ptr, in the same order as dbColumns, ready to splat into row.Scan(...).
func dbScanTargets(ptr any) []any {
	val := reflect.ValueOf(ptr).Elem()
	t := val.Type()
	var targets []any
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("db")
		if tag == "" || tag == "-" {
			continue
		}
		targets = append(targets, val.Field(i).Addr().Interface())
	}
	return targets
}
