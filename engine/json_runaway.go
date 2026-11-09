package engine

import "strings"

// runawayWhitespaceRun is the trailing whitespace run that marks degenerate decoding.
// Pretty-printed JSON never has more than a few dozen consecutive whitespace bytes
// (nesting depth times the indent), so a run this long is the model stuck emitting
// padding that a strict-schema decoder still allows, until the output cap.
const runawayWhitespaceRun = 256

// trimRunawayWhitespace cuts a trailing runaway run of whitespace off a cut-off JSON
// response. It reports whether it trimmed. What is left is the real JSON prefix, so a
// continuation turn resumes from it rather than from tens of thousands of tokens of
// padding that would be re-sent on every later call.
func trimRunawayWhitespace(content string) (string, bool) {
	trimmed := strings.TrimRight(content, " \t\r\n")
	if len(content)-len(trimmed) < runawayWhitespaceRun {
		return content, false
	}
	return trimmed, true
}

// compactJSONContinuation is the continuation prompt for a plan cut off by runaway
// whitespace: the generic "continue" plus the instruction that avoids a repeat.
const compactJSONContinuation = "Your JSON response was cut off after a long run of blank padding. Continue EXACTLY where the JSON above ended, output ONLY the remaining JSON to complete the object, and write it compactly: no indentation, no blank lines, no padding whitespace. No preamble, no explanation."

// compactJSONRestart is the same for a response that was nothing but padding, so there
// is no JSON prefix to continue from.
const compactJSONRestart = "Your last turn was only blank padding. Emit the FULL JSON object now, written compactly: no indentation, no blank lines, no padding whitespace. No preamble, no explanation."
