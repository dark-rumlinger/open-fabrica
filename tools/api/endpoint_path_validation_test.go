package api

import (
	"testing"

	"openfabrica/tools"
)

// TestEndpointCreateHandlersRejectBadPath proves the feeds/export/import/upload
// create handlers run the shared endpoint-path validation (charset + reserved
// suffix), matching the newer API/websocket/LLM tools. The check runs before any
// DB access, so a bare context is enough to exercise it.
func TestEndpointCreateHandlersRejectBadPath(t *testing.T) {
	ctx := &tools.ToolContext{}
	cases := []struct {
		name string
		call func() (*tools.Result, error)
	}{
		{"export/reserved-suffix", func() (*tools.Result, error) {
			return apiCreateExport(ctx, "data/export", "t", nil, false, "")
		}},
		{"export/bad-charset", func() (*tools.Result, error) {
			return apiCreateExport(ctx, "bad!path", "t", nil, false, "")
		}},
		{"import/reserved-suffix", func() (*tools.Result, error) {
			return apiCreateImport(ctx, "rows/import", "t", false, "", "", 0)
		}},
		{"upload/reserved-suffix", func() (*tools.Result, error) {
			return epCreateUpload(ctx, "files/upload", "t", false, 0, nil, 0, "")
		}},
		{"feed/bad-charset", func() (*tools.Result, error) {
			return feedCreate(ctx, &FeedCreate{EndpointPath: "bad!path", TableName: "t"})
		}},
	}
	for _, tc := range cases {
		res, err := tc.call()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if res == nil || res.Success {
			t.Errorf("%s: expected the shared path validation to reject the path, got %+v", tc.name, res)
		}
	}
}
