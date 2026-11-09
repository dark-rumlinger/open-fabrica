// Package engine implements the LLM-backed build pipeline: expand -> plan ->
// build, plus a live stage driven out-of-band by owner chat wakes. One unified
// session loop (session.go) is shared by every stage, with byte-stable prompt
// rendering for provider prompt caches and a content-addressed replay cache
// (of_llm_cache) that makes crash resume and retries free.
//
// Pluggability points: stages (RegisterStage, stage.go), tools (tools.Registry),
// validators (RegisterValidator, validator.go), guards (SessionSpec.Guard),
// guides (the pack package over knowledge/, rendered into stage prompts).
package engine
