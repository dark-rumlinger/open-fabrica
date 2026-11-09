package tools

import (
	"bytes"
	"encoding/json"
	"sort"
)

// resultPriorityKeys are the result-data fields that carry an instruction or a size
// warning: a directive to stop, the failure reason, a hint, a truncation flag. A Go map
// marshals alphabetically, so without this a field like `stop` sorts after a bulky
// `failures` list and is the first thing a tail truncation (TruncateResult) cuts, along
// with the loop-breaker it exists to deliver. They are written first, in this order.
var resultPriorityKeys = []string{
	"stop", "error", "warning", "warnings", "hint", "note",
	"replays_left", "replays_without_progress", "weak_checks",
	"truncated", "has_more", "count", "total",
}

// priorityMap is a result Data object that serializes resultPriorityKeys first and the
// remaining keys sorted, as a map would. HTML is not escaped, matching
// MarshalToolResult.
type priorityMap map[string]any

func (m priorityMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	written := make(map[string]bool, len(m))
	first := true
	emit := func(k string) error {
		v, ok := m[k]
		if !ok || written[k] {
			return nil
		}
		written[k] = true
		if !first {
			buf.WriteByte(',')
		}
		first = false
		kb, err := encodeNoEscape(k)
		if err != nil {
			return err
		}
		vb, err := encodeNoEscape(v)
		if err != nil {
			return err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
		return nil
	}
	for _, k := range resultPriorityKeys {
		if err := emit(k); err != nil {
			return nil, err
		}
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		if !written[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if err := emit(k); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func encodeNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
