package llm

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// sseMaxLineBytes caps the size of a single SSE event line. The bufio.Scanner
// default of 64KB is too small: Anthropic streams tool-call arguments as
// "input_json_delta" events that can carry several hundred KB of JSON in a
// single line when the model emits a large argument blob. 4MB keeps memory
// bounded while leaving plenty of headroom.
const sseMaxLineBytes = 4 << 20

// ScanSSEData reads a Server-Sent Events stream and invokes onData for each
// payload following a "data: " prefix. Comment lines (":"-prefixed), empty lines,
// and non-data fields are silently skipped. Stops early with nil if onData
// returns io.EOF, which providers use to short-circuit on a "[DONE]" sentinel or
// terminal event without surfacing it as a stream error.
//
// providerTag is prefixed onto any scanner error. onData receives the raw payload
// (after "data: " is stripped) and owns any JSON unmarshalling; it is never called
// with the literal "[DONE]" - callers detecting it should return io.EOF to stop.
func ScanSSEData(body io.Reader, providerTag string, onData func(data string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), sseMaxLineBytes)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if err := onData(data); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: reading stream: %w", providerTag, err)
	}
	return nil
}
