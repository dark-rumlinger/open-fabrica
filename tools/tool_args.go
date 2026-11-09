package tools

import (
	"encoding/json"
	"strconv"
	"strings"

	"openfabrica/internal/jsonrepair"
)

// RepairAndUnmarshalArgs parses a tool call's argument blob, repairing the
// loose-JSON artifacts the PLAN parser also tolerates and unwrapping the
// single-element array some providers emit, so such a call parses instead of
// burning a whole turn as a rejected call. Shared by every surface that
// dispatches model tool calls (engine and chat), so they tolerate the same input.
func RepairAndUnmarshalArgs(raw string) (map[string]any, error) {
	argBytes := []byte(jsonrepair.Repair(raw))
	if len(argBytes) > 0 && argBytes[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(argBytes, &arr); err == nil && len(arr) == 1 {
			argBytes = arr[0]
		}
	}
	var args map[string]any
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return nil, err
	}
	return args, nil
}

// ParseFindToolArgs reads find_tool's three args out of a repaired map, tolerating the
// shapes a model actually emits: a bare string where a list belongs, and a numeric limit
// that arrived as a JSON float or a string. Hand-decoded rather than json.Unmarshal'd into
// a struct because a strict decode of a slightly-off names value would throw away a call
// that is obvious in intent.
func ParseFindToolArgs(raw map[string]any) FindToolArgs {
	var out FindToolArgs
	switch v := raw["names"].(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			out.Names = []string{s}
		}
	case []any:
		for _, n := range v {
			if s, ok := n.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out.Names = append(out.Names, s)
				}
			}
		}
	}
	if q, ok := raw["query"].(string); ok {
		out.Query = q
	}
	switch v := raw["limit"].(type) {
	case float64:
		out.Limit = int(v)
	case int:
		out.Limit = v
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			out.Limit = n
		}
	}
	return out
}
