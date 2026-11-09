package api

// Small builders for the repeated OpenAPI fragments emitted by epGenerateDocs.
// The spec is assembled as map[string]any trees; pulling the two most
// repeated fragments (the BearerAuth security requirement and the {id} path
// parameter) into named helpers removes a dozen copies of the same deeply-nested
// literal and makes each call site read as intent rather than punctuation.
//
// These return fresh maps/slices on every call so callers can never alias and
// mutate a shared fragment.

// bearerSecurity is the OpenAPI security requirement selecting the BearerAuth
// scheme, i.e. `security: [ { BearerAuth: [] } ]`.
func bearerSecurity() []any {
	return []any{map[string]any{"BearerAuth": []any{}}}
}

// idPathParam is the `{id}` path parameter shared by every by-ID operation
// (GET one / PUT / DELETE): a required integer path parameter named "id".
func idPathParam() []any {
	return []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "integer"}}}
}

// schemaRef is a JSON Schema reference to a named component schema, i.e.
// `{ $ref: "#/components/schemas/<name>" }`.
func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// jsonContent wraps a schema as an `application/json` request/response body,
// i.e. `{ application/json: { schema: <schema> } }`.
func jsonContent(schema map[string]any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}

// jsonResp is a response object carrying a description and an application/json
// body, i.e. `{ description: <desc>, content: { application/json: ... } }`.
func jsonResp(desc string, schema map[string]any) map[string]any {
	return map[string]any{"description": desc, "content": jsonContent(schema)}
}

// secure attaches the BearerAuth security requirement to op when requiresAuth
// is true, then returns op so it can be assigned inline at the call site.
func secure(op map[string]any, requiresAuth bool) map[string]any {
	if requiresAuth {
		op["security"] = bearerSecurity()
	}
	return op
}
