package store

import (
	"strings"
	"testing"
)

// TestValidateAuthBootstrapUsage_ApiFetchEnvelope guards the rule that catches
// the "consumed apiFetch's Response as if it were parsed JSON" bug class - the
// defect that broke metrics.js ("Unable to load metrics") and users.js
// ("No users found").
func TestValidateAuthBootstrapUsage_ApiFetchEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "bug: read .data off apiFetch result, never parses",
			body: `apiFetch("metrics-overview").then(function (res) {
				var data = res && res.data;
				if (!data || !data.length) throw new Error("empty");
			});`,
			wantErr: true,
		},
		{
			name: "bug: read .count off apiFetch result, never parses",
			body: `apiFetch("users?" + params).then(function (res) {
				var rows = (res && res.data) || [];
				state.total = res.count || rows.length;
			});`,
			wantErr: true,
		},
		{
			name: "bug: read .rows off apiFetch result, never parses",
			body: `apiFetch("users").then(function (res) {
				var rows = res && res.rows;
				if (!rows || !rows.length) throw new Error("empty");
			});`,
			wantErr: true,
		},
		{
			name: "ok: parses with .json() before reading .data",
			body: `apiFetch("users").then(function (r) { return r.json(); })
				.then(function (res) { var rows = res.data || []; });`,
			wantErr: false,
		},
		{
			name: "ok: parses with .json() before reading .rows",
			body: `apiFetch("users").then(function (r) { return r.json(); })
				.then(function (res) { var rows = res.rows || []; rows.map(function (u) { return u.id; }); });`,
			wantErr: false,
		},
		{
			name: "ok: await (await apiFetch()).json() then destructure rows",
			body: `async function load() {
				const { rows } = await (await apiFetch("users")).json();
				rows.map(function (u) { return u.id; });
			}`,
			wantErr: false,
		},
		{
			name: "ok: await (await apiFetch()).json() one-liner",
			body: `async function load() {
				const { data } = await (await apiFetch("users")).json();
				data.map(function (u) { return u.id; });
			}`,
			wantErr: false,
		},
		{
			name: "ok: apiFetch for a POST that reads no envelope field",
			body: `apiFetch("users", { method: "POST", body: { name: n } })
				.then(function (r) { if (r.status === 201) done(); });`,
			wantErr: false,
		},
		{
			name: "ok: reads .data but uses raw fetch + .json (no apiFetch)",
			body: `fetch("/api/users").then(function (r) { return r.json(); })
				.then(function (res) { var rows = res.data; });`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAuthBootstrapUsage(&Artifact{Kind: "file", Path: "js/page.js", Body: tc.body})
			if tc.wantErr && err == nil {
				t.Fatalf("expected an envelope-misuse error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), ".json()") {
				t.Errorf("error should point at the .json() fix, got: %v", err)
			}
		})
	}
}
