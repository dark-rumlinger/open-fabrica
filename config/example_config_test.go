package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The public CSP is duplicated on purpose: as a literal in config.example.json so
// operators see the real default, and as DefaultPublicCSP in code. This guards the
// two against silent drift - a mismatch means one was edited without the other.
func TestExampleConfigCSPMatchesDefault(t *testing.T) {
	data, err := os.ReadFile("../config.example.json")
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	var example struct {
		PublicCSP string `json:"public_csp"`
	}
	if err := json.Unmarshal(data, &example); err != nil {
		t.Fatalf("parse config.example.json: %v", err)
	}
	if example.PublicCSP != DefaultPublicCSP {
		t.Errorf("public_csp in config.example.json does not match DefaultPublicCSP:\n example: %q\n default: %q",
			example.PublicCSP, DefaultPublicCSP)
	}
}

// TestExampleConfigKeysMatchStruct guards config.example.json against drift from
// the Config struct in both directions. "_"-prefixed keys are documentation-only;
// secret fields are intentionally absent from the example and allowlisted here.
func TestExampleConfigKeysMatchStruct(t *testing.T) {
	exampleOmits := map[string]bool{
		"jwt_secret":     true,
		"encryption_key": true,
	}

	data, err := os.ReadFile("../config.example.json")
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse config.example.json: %v", err)
	}

	structFields := map[string]bool{}
	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		structFields[strings.Split(tag, ",")[0]] = true
	}

	exampleKeys := map[string]bool{}
	for k := range raw {
		if strings.HasPrefix(k, "_") {
			continue // documentation-only keys (_note, _env_only_settings, ...)
		}
		exampleKeys[k] = true
		if !structFields[k] {
			t.Errorf("config.example.json documents key %q with no matching Config field", k)
		}
	}

	for name := range structFields {
		if exampleOmits[name] {
			continue
		}
		if !exampleKeys[name] {
			t.Errorf("Config field %q is not documented in config.example.json", name)
		}
	}
}
