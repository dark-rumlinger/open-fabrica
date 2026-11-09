package tools

import (
	"bytes"
	"encoding/json"
	"sort"
)

// OrderedProps is a JSON Schema "properties" object that serializes its keys in a given
// order. A Go map marshals alphabetically, and a model writing a tool call tends to emit
// the fields in the order the schema lists them: for a patch hunk that put `replace`
// before `search`, so the model wrote the new text before it had committed to the anchor
// and a fifth of all hunks came out with the two identical. Listing the anchor first fixes
// the order at the source.
type OrderedProps struct {
	Keys  []string
	Props map[string]any
}

// MarshalJSON writes Props with Keys first, in order, then any remaining keys sorted.
func (o OrderedProps) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	written := map[string]bool{}
	first := true
	emit := func(k string) error {
		v, ok := o.Props[k]
		if !ok || written[k] {
			return nil
		}
		written[k] = true
		if !first {
			buf.WriteByte(',')
		}
		first = false
		kb, err := json.Marshal(k)
		if err != nil {
			return err
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
		return nil
	}
	for _, k := range o.Keys {
		if err := emit(k); err != nil {
			return nil, err
		}
	}
	rest := make([]string, 0, len(o.Props))
	for k := range o.Props {
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
