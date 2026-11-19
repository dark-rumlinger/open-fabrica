package openai

import (
	"strings"
	"testing"

	"openfabrica/providers/llm"
)

// A stream ends with exactly one Done chunk, and a tool call still buffered
// when the stream ends without [DONE] is flushed before it.
func TestParseSSEStream_SingleDone(t *testing.T) {
	call := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`
	for name, tail := range map[string]string{"done": "data: [DONE]", "plain_eof": ""} {
		done, calls := 0, 0
		c := &Client{}
		err := c.parseSSEStream(strings.NewReader(call+"\n"+tail+"\n"), func(chunk llm.StreamChunk) {
			if chunk.ToolCall != nil {
				if done > 0 {
					t.Errorf("%s: tool call after Done", name)
				}
				calls++
			}
			if chunk.Done {
				done++
			}
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if done != 1 || calls != 1 {
			t.Errorf("%s: done=%d calls=%d, want 1 and 1", name, done, calls)
		}
	}
}
