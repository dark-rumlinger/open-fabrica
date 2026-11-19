package public

import "testing"

func TestStripHostPort(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"example.com", "example.com"},
		{"example.com:8080", "example.com"},
		{"127.0.0.1:5735", "127.0.0.1"},
		{"[::1]:5735", "::1"},
		{"[::1]", "::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
	}
	for _, tc := range tests {
		if got := stripHostPort(tc.in); got != tc.want {
			t.Errorf("stripHostPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
