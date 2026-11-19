package admin

import "testing"

func TestCleanProjectFilePath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"app.js", "app.js", true},
		{"/assets/css/app.css", "assets/css/app.css", true},
		{`assets\js\main.js`, "assets/js/main.js", true},
		{"", "", false},
		{"/", "", false},
		{".", "", false},
		{"..", "", false},
		{"../secret.txt", "", false},
		{"assets/../secret.txt", "", false},
		{"assets//app.css", "", false},
		{"assets/./app.css", "", false},
	}

	for _, tc := range tests {
		got, ok := cleanProjectFilePath(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("cleanProjectFilePath(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCleanProjectFolderPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"/", "", true},
		{"assets/css/", "assets/css", true},
		{"/assets/images", "assets/images", true},
		{"../etc", "", false},
		{"assets/../etc", "", false},
	}

	for _, tc := range tests {
		got, ok := cleanProjectFolderPath(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("cleanProjectFolderPath(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
