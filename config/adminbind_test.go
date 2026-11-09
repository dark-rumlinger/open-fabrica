package config

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{"", " ", "localhost", "LOCALHOST", "127.0.0.1", "127.0.0.5", "::1", "[::1]"}
	for _, h := range loopback {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}

	routable := []string{"0.0.0.0", "::", "192.168.1.10", "10.0.0.1", "203.0.113.7"}
	for _, h := range routable {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}

func TestAdminExposed(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":    false,
		"localhost":    false,
		"::1":          false,
		"0.0.0.0":      true,
		"::":           true,
		"192.168.0.50": true,
	}
	for addr, want := range cases {
		c := &Config{AdminBindAddr: addr}
		if got := c.AdminExposed(); got != want {
			t.Errorf("AdminExposed(%q) = %v, want %v", addr, got, want)
		}
	}
}
