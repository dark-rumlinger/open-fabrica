package tools

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Reflection helpers powering the struct-per-action registration shape in
// Define(). Each action is a struct whose fields ARE the JSON arg schema; this
// file converts between those two views:
//
//   schemaFromStruct  - struct type -> []Param + per-field required map
//   populateStruct    - args map     -> populated struct value
//   argsFromStruct    - struct value -> args map (for Example rendering)
//   deriveActionName  - type name    -> snake_case action name
//
// The tag vocabulary is intentionally tiny:
//   `arg:"name[,required]"`
//   `desc:"LLM-facing description (mandatory)"`
//   `enum:"a,b,c"`

// actionFieldInfo captures the per-field metadata extracted from struct tags.
type actionFieldInfo struct {
	GoField    string // exported Go field name, for reflect.Value.FieldByName
	ArgName    string // JSON arg key, from `arg:"<name>"`
	Type       string // JSON Schema type
	AltType    string // optional alt type (string OR object, etc.)
	Required   bool
	Enum       []string
	Desc       string
	Default    string // raw default string from `default:"..."` tag (empty = no default)
	HasDefault bool
	Min        float64 // lower bound from `min:"..."` (numeric args only)
	Max        float64 // upper bound from `max:"..."` (numeric args only)
	HasMin     bool
	HasMax     bool
	ItemsType  string // element type from `items:"..."` (array args only); overrides the Go-kind guess
}

// itemsTypes are the values an `items:"..."` tag may name: a JSON Schema element type,
// or "any" for a genuinely heterogeneous list (positional call arguments), which emits no
// items constraint at all rather than a wrong one.
var itemsTypes = map[string]bool{
	"string": true, "integer": true, "number": true, "boolean": true, "object": true, "any": true,
}

// extractActionFields walks the struct's visible exported fields - including
// fields promoted from embedded structs - and returns one entry per declared
// arg. Promotion still matters: some action types embed a shared arg struct
// (e.g. data, schema tools) and rely on the embedded struct's `arg:`-tagged
// fields appearing in the tool schema. Returns an error rather than panicking
// so the caller (Define) can surface registration problems through the
// existing recordRegistrationError channel.
func extractActionFields(t reflect.Type) ([]actionFieldInfo, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("action type %s is not a struct", t.Name())
	}
	var out []actionFieldInfo
	seenArg := make(map[string]string) // arg name -> first Go field that declared it
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() {
			continue
		}
		argTag, hasArg := f.Tag.Lookup("arg")
		if !hasArg {
			continue // fields without an `arg` tag are ignored (private state, embed wrappers)
		}
		name, required := parseArgTag(argTag)
		if name == "" {
			return nil, fmt.Errorf("%s.%s: empty arg name", t.Name(), f.Name)
		}
		if prev, dup := seenArg[name]; dup {
			// Two fields claiming the same arg key: the args map populates only
			// one of them (FieldByName order), silently dropping the other - a
			// forwarding-drift foot-gun. Fail at registration instead.
			return nil, fmt.Errorf("%s: duplicate arg name %q on fields %s and %s", t.Name(), name, prev, f.Name)
		}
		seenArg[name] = f.Name
		desc := f.Tag.Get("desc")
		if strings.TrimSpace(desc) == "" {
			return nil, fmt.Errorf("%s.%s: missing desc tag (LLM-facing description is mandatory)", t.Name(), f.Name)
		}
		jsonType, altType, err := goKindToJSONType(f.Type)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		var enum []string
		if e := f.Tag.Get("enum"); e != "" {
			for _, v := range strings.Split(e, ",") {
				v = strings.TrimSpace(v)
				if v != "" {
					enum = append(enum, v)
				}
			}
		}
		if len(enum) > 0 && jsonType != "string" {
			// Runtime enum validation is string-only, and a non-string enum
			// renders string members on a differently-typed schema param. Fail
			// at registration so the constraint moves to the handler instead of
			// silently not existing.
			return nil, fmt.Errorf("%s.%s: enum tag is only valid on string args, not %s", t.Name(), f.Name, jsonType)
		}
		defVal, hasDefault := f.Tag.Lookup("default")
		if hasDefault && required {
			return nil, fmt.Errorf("%s.%s: cannot combine required with default", t.Name(), f.Name)
		}
		minVal, hasMin, err := parseBoundTag(t, f, "min", jsonType)
		if err != nil {
			return nil, err
		}
		maxVal, hasMax, err := parseBoundTag(t, f, "max", jsonType)
		if err != nil {
			return nil, err
		}
		if hasMin && hasMax && minVal > maxVal {
			return nil, fmt.Errorf("%s.%s: min %v is greater than max %v", t.Name(), f.Name, minVal, maxVal)
		}
		itemsType, err := parseItemsTag(t, f, jsonType)
		if err != nil {
			return nil, err
		}
		out = append(out, actionFieldInfo{
			GoField:    f.Name,
			ArgName:    name,
			Type:       jsonType,
			AltType:    altType,
			Required:   required,
			Enum:       enum,
			Desc:       desc,
			Default:    defVal,
			HasDefault: hasDefault,
			Min:        minVal,
			Max:        maxVal,
			HasMin:     hasMin,
			HasMax:     hasMax,
			ItemsType:  itemsType,
		})
	}
	return out, nil
}

// parseItemsTag reads an `items:"<json type>"` override for an array arg. A []any field
// (used for args that tolerate loose model output) reflects to element kind Interface,
// which renders as {"type":"object"} - wrong for an arg that wants a bare string array.
// The tag lets the field stay []any while the schema states the true element type.
func parseItemsTag(t reflect.Type, f reflect.StructField, jsonType string) (string, error) {
	raw, ok := f.Tag.Lookup("items")
	if !ok {
		return "", nil
	}
	raw = strings.TrimSpace(raw)
	if jsonType != "array" {
		return "", fmt.Errorf("%s.%s: items tag is only valid on array args, not %s", t.Name(), f.Name, jsonType)
	}
	if !itemsTypes[raw] {
		return "", fmt.Errorf("%s.%s: items tag %q is not a JSON Schema type (string, integer, number, boolean, object)", t.Name(), f.Name, raw)
	}
	return raw, nil
}

// parseArgTag splits an `arg` tag value like "public_key,required" into name
// and required flag. Additional modifiers can be added later (e.g. "default=X")
// without breaking call sites.
func parseArgTag(tag string) (name string, required bool) {
	parts := strings.Split(tag, ",")
	name = strings.TrimSpace(parts[0])
	for _, mod := range parts[1:] {
		switch strings.TrimSpace(mod) {
		case "required":
			required = true
		}
	}
	return name, required
}

// parseBoundTag reads a `min:"..."`/`max:"..."` numeric bound off a struct
// field. Returns (value, present, error). A bound on a non-numeric arg
// (integer/number) is a registration error - the runtime can only bound-check
// numbers - so the mistake surfaces at boot rather than being silently ignored.
func parseBoundTag(t reflect.Type, f reflect.StructField, tagName, jsonType string) (float64, bool, error) {
	raw, ok := f.Tag.Lookup(tagName)
	if !ok {
		return 0, false, nil
	}
	if jsonType != "integer" && jsonType != "number" {
		return 0, false, fmt.Errorf("%s.%s: %s tag is only valid on integer/number args, not %s", t.Name(), f.Name, tagName, jsonType)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s.%s: invalid %s tag %q: %w", t.Name(), f.Name, tagName, raw, err)
	}
	return v, true, nil
}

// goKindToJSONType maps a reflect.Type to a JSON Schema "type" string and an
// optional alt-type (for fields that accept string-OR-object, mirroring the
// existing Param.AltType behaviour). Pointers are followed once; nil-vs-set
// is not modeled (Required handles "must be present").
func goKindToJSONType(t reflect.Type) (string, string, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string", "", nil
	case reflect.Bool:
		return "boolean", "", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer", "", nil
	case reflect.Float32, reflect.Float64:
		return "number", "", nil
	case reflect.Slice, reflect.Array:
		return "array", "", nil
	case reflect.Map, reflect.Struct:
		return "object", "", nil
	case reflect.Interface:
		// Bare interface{} fields are dual-typed: the handler decides at runtime.
		// Treat them as string|object - covers the common "raw body or JSON" use
		// case modeled by Param.AltType in the underlying ToolDef.
		return "string", "object", nil
	}
	return "", "", fmt.Errorf("unsupported Go kind %s for arg field", t.Kind())
}

// itemsSchemaFor returns the JSON Schema "items" sub-schema for a slice/array
// field type. Returns nil for non-array fields. Param.Items is a
// map[string]interface{}; we emit that exact shape so the rendered tool
// schema is the same one find_tool surfaces to the LLM.
func itemsSchemaFor(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
		return nil
	}
	elem := t.Elem()
	if elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	switch elem.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Map, reflect.Struct, reflect.Interface:
		return map[string]any{"type": "object"}
	}
	return nil
}

// fieldsToParams converts extracted field info into the Param slice the
// registry stores on ToolDef. Returns the per-action param slice (used by the
// per-action validator); the merged tool-level slice that Parameters() needs
// is assembled by Define from these per-action slices.
func fieldsToParams(t reflect.Type, fields []actionFieldInfo) []Param {
	ts := derefStruct(t)
	out := make([]Param, 0, len(fields))
	for _, fi := range fields {
		var items map[string]any
		if fi.Type == "array" {
			switch {
			case fi.ItemsType == "any":
				// Heterogeneous by design: no element constraint beats a wrong one.
				items = nil
			case fi.ItemsType != "":
				// An explicit `items:"..."` tag wins: see parseItemsTag for why the Go
				// kind cannot be trusted on the []any args.
				items = map[string]any{"type": fi.ItemsType}
			default:
				if sf, ok := ts.FieldByName(fi.GoField); ok {
					items = itemsSchemaFor(sf.Type)
				}
			}
		}
		out = append(out, Param{
			Name:        fi.ArgName,
			Type:        fi.Type,
			AltType:     fi.AltType,
			Description: fi.Desc,
			Required:    fi.Required,
			Enum:        fi.Enum,
			Items:       items,
			Default:     fi.Default,
			HasDefault:  fi.HasDefault,
			Min:         fi.Min,
			Max:         fi.Max,
			HasMin:      fi.HasMin,
			HasMax:      fi.HasMax,
		})
	}
	return out
}

func derefStruct(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

// populateActionStruct returns a freshly-allocated *T populated from args.
// Only fields whose `arg` key is present in args are touched; everything
// else keeps its zero value. Per-action required-ness is enforced separately
// by validateActionArgs before this is called.
func populateActionStruct(t reflect.Type, fields []actionFieldInfo, args map[string]any) (reflect.Value, error) {
	ts := derefStruct(t)
	ptr := reflect.New(ts) // *T
	v := ptr.Elem()
	for _, fi := range fields {
		fv := v.FieldByName(fi.GoField)
		if !fv.IsValid() || !fv.CanSet() {
			continue
		}
		raw, present := args[fi.ArgName]
		if !present || raw == nil {
			// Field absent: apply default if declared, otherwise leave the
			// zero value. Defaults are parsed from the string-form tag here
			// so handlers see the typed value already populated.
			if fi.HasDefault {
				if err := applyDefault(fv, fi.Default); err != nil {
					return reflect.Value{}, fmt.Errorf("default for %q: %w", fi.ArgName, err)
				}
			}
			continue
		}
		if err := assignReflect(fv, raw); err != nil {
			return reflect.Value{}, fmt.Errorf("arg %q: %w", fi.ArgName, err)
		}
		if err := checkNumericBound(fv, fi); err != nil {
			return reflect.Value{}, fmt.Errorf("arg %q: %w", fi.ArgName, err)
		}
	}
	return ptr, nil
}

// checkNumericBound validates an integer/number field against its declared
// [min, max] bounds, returning an error when the value is out of range instead
// of silently clamping it. A declared max:"200" is a hard limit: the model
// learns the real bound from the rejection rather than getting a quietly
// truncated value. Tri-state pointer fields are dereferenced first, so a bound
// on a *int (nil = unset) is enforced too. A no-op when no bound is declared or
// the field is not numeric.
func checkNumericBound(fv reflect.Value, fi actionFieldInfo) error {
	if !fi.HasMin && !fi.HasMax {
		return nil
	}
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil
		}
		fv = fv.Elem()
	}
	var n float64
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n = float64(fv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n = float64(fv.Uint())
	case reflect.Float32, reflect.Float64:
		n = fv.Float()
	default:
		return nil
	}
	if fi.HasMax && n > fi.Max {
		return fmt.Errorf("must be <= %g (got %g)", fi.Max, n)
	}
	if fi.HasMin && n < fi.Min {
		return fmt.Errorf("must be >= %g (got %g)", fi.Min, n)
	}
	return nil
}

// applyDefault parses the string-form default tag and sets it on the field
// using the field's Go type. Strings and bools pass through; numbers parse
// via strconv. Slices accept comma-separated values. Unknown kinds error so
// drift surfaces at registration.
func applyDefault(fv reflect.Value, raw string) error {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
		return nil
	case reflect.Bool:
		switch strings.ToLower(raw) {
		case "true", "1", "yes":
			fv.SetBool(true)
		case "false", "0", "no", "":
			fv.SetBool(false)
		default:
			return fmt.Errorf("invalid bool %q", raw)
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return err
		}
		fv.SetUint(n)
		return nil
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		fv.SetFloat(n)
		return nil
	case reflect.Slice:
		if raw == "" {
			return nil
		}
		parts := strings.Split(raw, ",")
		slice := reflect.MakeSlice(fv.Type(), len(parts), len(parts))
		for i, p := range parts {
			if err := applyDefault(slice.Index(i), strings.TrimSpace(p)); err != nil {
				return fmt.Errorf("index %d: %w", i, err)
			}
		}
		fv.Set(slice)
		return nil
	}
	return fmt.Errorf("unsupported kind %s for default", fv.Kind())
}

// assignReflect sets a struct field from a JSON-decoded value. Mirrors the
// JSON-into-Go type rules used by validateArgs: numbers arrive as float64,
// arrays as []interface{}, objects as map[string]interface{}.
func assignReflect(fv reflect.Value, raw any) error {
	// A JSON null. populateActionStruct drops a top-level null before calling
	// here, but a null ELEMENT inside an array (`args:[1,null]` decodes to
	// []interface{}{1.0, nil}) reaches this recursively - and reflect.ValueOf(nil)
	// is a zero Value, so the Interface/Map cases below would panic ("reflect:
	// call of reflect.Value.Set on zero Value") that only a recover() catches.
	// Leave the field/element at its zero value (a nil interface / pointer / the
	// type's zero), the correct JSON-null mapping that round-trips back to null.
	if raw == nil {
		return nil
	}
	// Pointer fields opt into tristate semantics (nil = "not set"). Allocate
	// the element and recurse so *bool, *int, etc. all populate correctly.
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		return assignReflect(fv.Elem(), raw)
	}
	switch fv.Kind() {
	case reflect.String:
		if s, ok := raw.(string); ok {
			fv.SetString(s)
			return nil
		}
	case reflect.Bool:
		if b, ok := raw.(bool); ok {
			fv.SetBool(b)
			return nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if f, ok := raw.(float64); ok {
			fv.SetInt(int64(f))
			return nil
		}
		if i, ok := raw.(int); ok {
			fv.SetInt(int64(i))
			return nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if f, ok := raw.(float64); ok {
			fv.SetUint(uint64(f))
			return nil
		}
	case reflect.Float32, reflect.Float64:
		if f, ok := raw.(float64); ok {
			fv.SetFloat(f)
			return nil
		}
	case reflect.Slice:
		if arr, ok := raw.([]any); ok {
			slice := reflect.MakeSlice(fv.Type(), len(arr), len(arr))
			for i, el := range arr {
				if err := assignReflect(slice.Index(i), el); err != nil {
					return fmt.Errorf("index %d: %w", i, err)
				}
			}
			fv.Set(slice)
			return nil
		}
	case reflect.Map:
		if m, ok := raw.(map[string]any); ok {
			fv.Set(reflect.ValueOf(m))
			return nil
		}
	case reflect.Interface:
		fv.Set(reflect.ValueOf(raw))
		return nil
	}
	return fmt.Errorf("cannot assign %T to %s", raw, fv.Type())
}

// PopulateAction fills a (a pointer to an Action struct) from args using the
// same tag-driven decode the registered tool loop uses, so an out-of-loop
// dispatcher (e.g. the manage_sql transaction decoding its inner-op maps)
// shares the one arg system instead of hand-extracting map keys. Defaults and
// min/max bounds apply; required-ness stays with the caller's own checks.
func PopulateAction(a Action, args map[string]any) error {
	t := reflect.TypeOf(a)
	fields, err := extractActionFields(derefStruct(t))
	if err != nil {
		return err
	}
	ptr, err := populateActionStruct(t, fields, args)
	if err != nil {
		return err
	}
	reflect.ValueOf(a).Elem().Set(ptr.Elem())
	return nil
}

// argsFromStruct produces the JSON-shaped args map for an Example value.
// Skips zero-valued fields so the example only shows what the writer set.
// Always prefixes with "action": <actionName> so the example is a complete,
// valid tool call ready for the LLM to pattern-match.
func argsFromStruct(actionName string, v any, fields []actionFieldInfo) map[string]any {
	out := map[string]any{"action": actionName}
	if v == nil {
		return out
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return out
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return out
	}
	for _, fi := range fields {
		fv := rv.FieldByName(fi.GoField)
		if !fv.IsValid() || fv.IsZero() {
			continue
		}
		// Dereference pointer-form fields so the example map carries plain
		// JSON-shaped values (true, not *bool). assignReflect re-allocates
		// the pointer on the receive side.
		if fv.Kind() == reflect.Pointer {
			fv = fv.Elem()
		}
		out[fi.ArgName] = fv.Interface()
	}
	return out
}

// deriveActionName turns a struct type name into a snake_case action name.
// If the type name begins with the tool name's CamelCase form (with any
// "manage_" prefix stripped from the tool name first), strip that prefix
// before snake-casing. Examples:
//
//	tool "manage_tenancy", type "TenancyEnable"     -> "enable"
//	tool "manage_2fa",     type "TwoFactorEnable"   -> "two_factor_enable"
//	tool "data_query",     type "DataQueryRun"      -> "run"
//
// Actions implementing actionNamer (Name() string) override this entirely.
func deriveActionName(toolName, typeName string) string {
	base := strings.TrimPrefix(toolName, "manage_")
	camel := snakeToCamel(base)
	// Try exact prefix, then a singular variant (drop a trailing "s") so a
	// "manage_files" tool matches "FileAttach". English-only heuristic,
	// but covers every plural tool name in this repo.
	candidates := []string{camel}
	if strings.HasSuffix(camel, "s") {
		candidates = append(candidates, strings.TrimSuffix(camel, "s"))
	}
	for _, c := range candidates {
		if c == "" || len(typeName) <= len(c) {
			continue
		}
		if strings.EqualFold(typeName[:len(c)], c) {
			typeName = typeName[len(c):]
			break
		}
	}
	return camelToSnake(typeName)
}

func snakeToCamel(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	upper := true
	for _, r := range s {
		if r == '_' {
			upper = true
			continue
		}
		if upper {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
			upper = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

func camelToSnake(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r = r - 'A' + 'a'
		}
		b.WriteRune(r)
	}
	return b.String()
}
