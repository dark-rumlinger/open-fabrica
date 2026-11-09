package engine

// JSON schema for the PLAN stage response. Providers that honour ResponseSchema use it
// to force schema-clean output, eliminating the markdown-fence / trailing-comma /
// wrapper-array repair paths. The structural skeleton is pinned: the root and each
// task/library item set additionalProperties:false, so a grammar-constrained provider
// can only emit known property names (else the decoder accepts an arbitrary string as a
// name and drifts into unparseable JSON); free-text values stay open strings. The root
// anyOf requires a build queue OR a question, so a model can't close the object with an
// empty queue, and stays a plain object (not a top-level anyOf) because the same schema
// is reused as a forced-tool input_schema and a response_format, both needing an object
// root. No post-decode shape validation beyond that; the loop tolerates whatever parses.
//
// Descriptions NAME each field; they never teach how to decide it. This schema is pinned
// onto the same request as the assembled PLAN frame (stage_plan.go), so any judgment
// written here is the frame's rule a second time, in different words, in one prompt - and
// it is re-sent on every coherence retry. The frame owns the judgment; keep these terse.
// TestPlanSchemaDescriptionBudget prices a regrowth.

import (
	"encoding/json"

	"openfabrica/providers/llm"
)

// PlanResponseSchema returns the *llm.ResponseSchema that the PLAN stage
// pins onto its CompletionRequest. Returned by value so callers do not
// share the underlying RawMessage by accident.
func PlanResponseSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Name:   "plan_response",
		Schema: json.RawMessage(planSchemaJSON),
		Strict: false,
	}
}

// planSchemaJSON is the literal JSON Schema document sent to providers.
// Kept as a string constant so the schema lives in one place and the
// runtime never re-marshals an in-memory struct on every PLAN call.
const planSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "anyOf": [
    {"required": ["tasks"]},
    {"required": ["questions"]}
  ],
  "properties": {
    "goal":   {"type": "string", "description": "One or two sentences: what this project is."},
    "intro":  {"type": "string", "description": "Short, warm chat message shown to the owner when the plan is ready."},
    "voice":  {"type": "string", "description": "Editorial voice for all copy. Free text."},
    "theme":  {"type": "string", "description": "One or two sentences: the audience, mood and emotion the look must fit, and why the ground (light or dark) and palette suit it."},
    "design": {"type": "string", "description": "Prose description of the visual direction. Free text."},
    "design_system": {
      "type": "object",
      "additionalProperties": false,
      "description": "The look as concrete values. Fill it whenever the project has a UI.",
      "required": ["colors", "font_body", "motion"],
      "properties": {
        "colors": {
          "type": "object",
          "additionalProperties": false,
          "description": "Palette by role, hex.",
          "required": ["primary", "bg", "surface", "text"],
          "properties": {
            "primary": {"type": "string"},
            "bg":      {"type": "string"},
            "surface": {"type": "string"},
            "text":    {"type": "string"},
            "accent":  {"type": "string"},
            "muted":   {"type": "string"}
          }
        },
        "font_body":    {"type": "string", "description": "Body family; match a declared font library or a system stack."},
        "font_display": {"type": "string", "description": "Heading family."},
        "type_scale":   {"type": "string"},
        "spacing_base": {"type": "string"},
        "radius":       {"type": "string"},
        "elevation":    {"type": "string"},
        "motion":       {"type": "string"}
      }
    },
    "tasks": {
      "type": "array",
      "minItems": 1,
      "description": "The build checklist, in order.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "id":        {"type": "string", "description": "Stable slug. Required only when another task's deps reference it; omit to derive one from the title."},
          "title":     {"type": "string", "description": "Short imperative title."},
          "intent":    {"type": "string", "description": "What to build, in prose."},
          "done_when": {"type": "string", "description": "Verifiable completion criteria for this task."},
          "deps":      {"type": "array", "items": {"type": "string"}, "description": "Ids of tasks that must finish first."},
          "paths":     {"type": "array", "items": {"type": "string"}, "description": "Durable artifacts this task OWNS: page routes, tables, endpoint resources - plus, for a subsystem split only, its own module files."},
          "edits":     {"type": "array", "items": {"type": "string"}, "description": "The route/resource/file this task EXTENDS but another task owns."},
          "exports":   {"type": "array", "items": {"type": "string"}, "description": "Subsystem splits only: the named symbols this task's module(s) export for sibling tasks to import. One symbol per entry, as one terse signature."},
          "tools":     {"type": "array", "items": {"type": "string"}, "description": "Non-core tools this task needs (Build tool catalogue)."},
          "covers":    {"type": "array", "items": {"type": "string"}, "description": "Feature ids it builds."}
        },
        "required": ["title", "intent", "done_when"]
      }
    },
    "external_libraries": {
      "type": "array",
      "description": "CDN libraries the project needs. The platform auto-injects the tag from the URL.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "name":        {"type": "string"},
          "url":         {"type": "string"},
          "purpose":     {"type": "string"},
          "kind":        {"type": "string", "enum": ["", "classic", "module"], "description": "JS module system only: \"classic\" (UMD/IIFE) or \"module\" (ESM). Omit for CSS/font URLs."},
          "module_name": {"type": "string", "description": "Required when kind=\"module\": the bare specifier used in imports (e.g. \"three\", or \"three/addons/\" for a subpath prefix)."},
          "global_name": {"type": "string", "description": "Required when kind=\"classic\": the exact window global the script defines (e.g. \"Chart\", \"marked\")."},
          "integrity":   {"type": "string"}
        }
      }
    },
    "guides":     {"type": "array", "items": {"type": "string"}, "description": "Guide slugs the BUILD needs beyond what this plan's tools and libraries already imply. Unknown slugs are ignored."},
    "exclusions": {"type": "array", "items": {"type": "string"}, "description": "Things explicitly NOT to build."},
    "decisions": {
      "type": "array",
      "description": "Locked architecture choices BUILD must honor exactly.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "topic":     {"type": "string", "description": "The choice being made, e.g. \"realtime model\"."},
          "choice":    {"type": "string", "description": "The option picked, e.g. \"tick\", \"authoritative\", \"relay\"."},
          "rationale": {"type": "string", "description": "One line on why, tied to the brief."},
          "check":     {
            "type": "object",
            "additionalProperties": false,
            "description": "A machine-checkable assertion BUILD is held to. Copy it verbatim from a guide's '## Decision:' block; omit it when the guide gives none.",
            "properties": {
              "artifact": {"type": "string", "description": "Artifact type: websocket | trigger | endpoint."},
              "id":       {"type": "string", "description": "Endpoint path, or a trigger's event_type."},
              "field":    {"type": "string", "description": "The property to check: websocket -> tick_hz | authoritative | matchmaking | model; trigger -> count; endpoint -> auth_model."},
              "op":       {"type": "string", "enum": ["gt", "gte", "lt", "lte", "eq", "ne", "exists", "nexists"]},
              "value":    {"description": "The comparand."}
            }
          }
        },
        "required": ["topic", "choice"]
      }
    },
    "questions": {
      "type": "array",
      "minItems": 1,
      "description": "Up to 2 clarifying questions for the owner. Only ask when the answer materially changes the build.",
      "items": {
        "type": "object",
        "additionalProperties": true,
        "properties": {
          "id":          {"type": "string"},
          "question":    {"type": "string"},
          "type":        {"type": "string", "enum": ["text", "single_choice", "multi_choice", "secret", "form"]},
          "options":     {"type": "array", "items": {"type": "string"}},
          "secret_name": {"type": "string"},
          "fields":      {}
        },
        "required": ["question"]
      }
    }
  }
}`
