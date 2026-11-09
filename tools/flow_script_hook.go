package tools

// FlowScriptSaver saves and replays the project's user-flow script. The tool that owns the
// script registers it, so another tool can hand it an edit without importing the owner.
var FlowScriptSaver func(ctx *ToolContext, source string) (*Result, error)
