package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMeasuredToolsRejectMalformedArgumentsOverHTTP(t *testing.T) {
	s, _, _ := liveServer(t)
	client, _ := connectClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, raw := range []string{
		`{}`,
		`{"stream_id":"x","catalog_revision":"x","steps":[],"timeout_ms":0,"expect":{"stream_id":"y","signal_id":"z","edge":"any"}}`,
		`{"stream_id":"x","catalog_revision":"x","steps":[{"after_ms":0,"changes":[{"signal_id":"a","rms":1200}]}],"timeout_ms":5000,"expect":{"stream_id":"y","signal_id":"z","edge":"rising"},"typo":true}`,
		`{"stream_id":"x","catalog_revision":"x","steps":[{"after_ms":0.5,"changes":[]}],"timeout_ms":5000,"expect":{"stream_id":"y","signal_id":"z","edge":"rising"}}`,
	} {
		r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "run_test", Arguments: json.RawMessage(raw)})
		if err == nil && !r.IsError {
			t.Fatal("invalid test accepted", raw)
		}
	}
	for _, tool := range []string{"get_test", "cancel_test"} {
		r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"test_id": "missing"}})
		if err != nil || !r.IsError {
			t.Fatal("unknown test ID was not rejected", tool, r, err)
		}
	}
}
