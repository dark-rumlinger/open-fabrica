package main

import "testing"

func TestParseByteSize(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"0", 0},
		{"512", 512},
		{"1k", 1000},
		{"1kb", 1000},
		{"1KB", 1000},
		{"1KiB", 1024},
		{"2MiB", 2 * 1024 * 1024},
		{"1mb", 1_000_000},
		{"1GiB", 1 << 30},
		{"1.5GiB", int64(1.5 * (1 << 30))},
		{" 256 MiB ", 256 * 1024 * 1024},
	}
	for _, tc := range ok {
		got, err := parseByteSize(tc.in)
		if err != nil {
			t.Errorf("parseByteSize(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	bad := []string{"", "abc", "10 zb", "GiB", "1.2.3"}
	for _, in := range bad {
		if _, err := parseByteSize(in); err == nil {
			t.Errorf("parseByteSize(%q) = nil error, want an error", in)
		}
	}
}
