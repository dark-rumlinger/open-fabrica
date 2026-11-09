package public

import (
	"encoding/json"
	"strings"
)

// structuralSchemaCheck reports whether content is JSON whose top-level shape
// matches the declared schema's top-level `type` (and, for objects, that every
// `required` key is present). It is deliberately shallow - catching the common
// weak-model failures (a bare array when an object was asked for, prose instead of
// JSON, a missing required key) without a full JSON-schema validator.
//
// It is conservative: when the schema can't be understood it returns true, so a
// malformed stored schema never blocks an otherwise-fine reply.
func structuralSchemaCheck(content, schema string) bool {
	var sch map[string]json.RawMessage
	if err := json.Unmarshal([]byte(schema), &sch); err != nil {
		return true // unintelligible schema: don't block
	}
	var typ string
	if raw, ok := sch["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}

	trimmed := strings.TrimSpace(content)
	switch typ {
	case "object":
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return false // not a JSON object (e.g. a bare array or prose)
		}
		if raw, ok := sch["required"]; ok {
			var req []string
			if json.Unmarshal(raw, &req) == nil {
				for _, k := range req {
					if _, present := obj[k]; !present {
						return false
					}
				}
			}
		}
		return true
	case "array":
		var arr []json.RawMessage
		return json.Unmarshal([]byte(trimmed), &arr) == nil
	case "string", "number", "integer", "boolean", "null":
		var v any
		return json.Unmarshal([]byte(trimmed), &v) == nil
	default:
		return true // no usable top-level type declared: don't block
	}
}

// pinSchemaRails returns the schema with additionalProperties:false set on every
// object node that declares properties and does not already say otherwise, so a
// grammar-constrained provider can only emit property names the schema names. That
// rail holds a weaker model inside the envelope when a long string value tempts it
// out, and is half of what makes a schema eligible for strict mode - an authored
// schema almost never carries it, so without this pass structured output degrades to
// a hint.
//
// A node that declares no properties is an open map by intent and is left alone, as
// is one that already sets additionalProperties. A non-object document is returned
// unchanged.
func pinSchemaRails(schema string) string {
	if strings.TrimSpace(schema) == "" {
		return schema
	}
	var node map[string]any
	if err := json.Unmarshal([]byte(schema), &node); err != nil {
		return schema
	}
	pinNodeRails(node)
	out, err := json.Marshal(node)
	if err != nil {
		return schema
	}
	return string(out)
}

// pinNodeRails pins one schema node and walks every keyword that carries a
// subschema, so a nested object gets the same rail as the root.
func pinNodeRails(node map[string]any) {
	props, _ := node["properties"].(map[string]any)
	if len(props) > 0 {
		if _, set := node["additionalProperties"]; !set {
			node["additionalProperties"] = false
		}
	}
	for _, child := range props {
		pinSubschema(child)
	}
	for _, key := range []string{"items", "additionalProperties", "not"} {
		pinSubschema(node[key])
	}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := node[key].(map[string]any); ok {
			for _, child := range defs {
				pinSubschema(child)
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if list, ok := node[key].([]any); ok {
			for _, child := range list {
				pinSubschema(child)
			}
		}
	}
}

// pinSubschema descends into a keyword's value, which is a schema, a tuple of
// schemas, or (for a pinned additionalProperties) a plain bool to skip.
func pinSubschema(v any) {
	switch t := v.(type) {
	case map[string]any:
		pinNodeRails(t)
	case []any:
		for _, e := range t {
			pinSubschema(e)
		}
	}
}

// schemaIsStrictCompatible reports whether the schema satisfies OpenAI's strict
// json_schema rules at every object level: additionalProperties:false and every
// declared property also listed in `required`. A schema that already satisfies
// this can be pinned with strict:true so providers that genuinely constrain
// decoding are forced to conform; one that doesn't is sent non-strict so those
// providers don't reject it outright (the CodeSchemaUnsupported fallback still
// covers model-level rejection either way).
func schemaIsStrictCompatible(schema string) bool {
	var node map[string]json.RawMessage
	if err := json.Unmarshal([]byte(schema), &node); err != nil {
		return false
	}
	return nodeStrictCompatible(node)
}

func nodeStrictCompatible(node map[string]json.RawMessage) bool {
	var typ string
	if raw, ok := node["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}

	switch typ {
	case "object":
		// additionalProperties must be explicitly false.
		ap, ok := node["additionalProperties"]
		if !ok {
			return false
		}
		var apBool bool
		if json.Unmarshal(ap, &apBool) != nil || apBool {
			return false
		}

		var props map[string]json.RawMessage
		if raw, ok := node["properties"]; ok {
			if json.Unmarshal(raw, &props) != nil {
				return false
			}
		}
		var req []string
		if raw, ok := node["required"]; ok {
			_ = json.Unmarshal(raw, &req)
		}
		reqSet := make(map[string]bool, len(req))
		for _, k := range req {
			reqSet[k] = true
		}
		for name, praw := range props {
			if !reqSet[name] {
				return false // every property must be required under strict mode
			}
			var child map[string]json.RawMessage
			if json.Unmarshal(praw, &child) != nil {
				return false
			}
			if !nodeStrictCompatible(child) {
				return false
			}
		}
		return true
	case "array":
		if raw, ok := node["items"]; ok {
			var items map[string]json.RawMessage
			if json.Unmarshal(raw, &items) != nil {
				return false
			}
			return nodeStrictCompatible(items)
		}
		return true
	default:
		return true // scalar leaf
	}
}
