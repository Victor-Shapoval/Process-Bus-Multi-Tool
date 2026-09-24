package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"pbmt/internal/application/automation"
	"pbmt/internal/application/catalog"
	"pbmt/internal/domain/goose"
)

func TestTimedGooseOverHTTP(t *testing.T) {
	for _, ending := range []string{"cancel", "stop", "disconnect"} {
		t.Run(ending, func(t *testing.T) {
			s, p, _ := liveServer(t)
			client, _ := connectClient(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var stream catalog.Stream
			for _, module := range s.api.Catalog.Modules {
				if module.ID == "goose_pub" {
					stream = module.Streams[0]
				}
			}
			p.gooseMu.Lock()
			for _, sig := range stream.Signals {
				v := goose.DataValue{Type: goose.DataTypeBoolean}
				if sig.Type == "Quality" {
					v = goose.NewQualityBitString(0)
				}
				p.goose = append(p.goose, v)
			}
			p.gooseMu.Unlock()
			args := map[string]any{
				"stream_id": stream.ID, "catalog_revision": s.api.Catalog.Revision,
				"steps": []any{
					map[string]any{"after_ms": 0, "changes": []any{map[string]any{"signal_id": stream.Signals[0].ID, "value": true}}},
					map[string]any{"after_ms": 86400000, "changes": []any{map[string]any{"signal_id": stream.Signals[0].ID, "value": false}}},
				},
			}
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "t_set_goose", Arguments: args})
			if err != nil || result.IsError {
				t.Fatal("start failed", result, err)
			}
			decode := func(result *mcp.CallToolResult) automation.SequenceStatus {
				t.Helper()
				data, err := json.Marshal(result.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				var state automation.SequenceStatus
				if err := json.Unmarshal(data, &state); err != nil {
					t.Fatal(err)
				}
				return state
			}
			state := decode(result)
			if state.ID == "" || state.State != "running" {
				t.Fatal("start did not return asynchronous job", state)
			}
			for state.StepsApplied == 0 {
				if ctx.Err() != nil {
					t.Fatal("first step never applied")
				}
				result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "get_sequence", Arguments: map[string]any{"sequence_id": state.ID}})
				if err != nil || result.IsError {
					t.Fatal(result, err)
				}
				state = decode(result)
				if state.State != "running" {
					t.Fatal("sequence unexpectedly ended", state)
				}
				time.Sleep(time.Millisecond)
			}
			status, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_status", Arguments: map[string]any{}})
			if err != nil || status.IsError {
				t.Fatal(status, err)
			}
			data, _ := json.Marshal(status.StructuredContent)
			var view struct {
				Sequences []automation.SequenceStatus `json:"sequences"`
			}
			if err := json.Unmarshal(data, &view); err != nil || len(view.Sequences) != 1 || view.Sequences[0].ID != state.ID {
				t.Fatal("get_status omits sequence", string(data), err)
			}
			switch ending {
			case "cancel":
				result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "cancel_sequence", Arguments: map[string]any{"sequence_id": state.ID}})
				if err != nil || result.IsError || decode(result).State != "cancelled" {
					t.Fatal("cancel failed", result, err)
				}
				if !s.Status().Running || p.closes.Load() != 0 || p.releases.Load() != 0 {
					t.Fatal("cancel stopped MCP or publishers")
				}
			case "stop":
				if err := s.Stop(); err != nil || p.releases.Load() != 1 || p.closes.Load() != 0 {
					t.Fatal("Stop MCP did not hand over", err)
				}
			case "disconnect":
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
				waitStopped(t, s, p)
			}
			if p.writes.Load() != 1 {
				t.Fatal("unexpected extra write")
			}
		})
	}
}

func TestTimedToolsRejectMalformedArguments(t *testing.T) {
	s, _, _ := liveServer(t)
	client, _ := connectClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, name := range []string{"t_set_sv", "t_set_goose"} {
		for _, raw := range []string{
			`{"stream_id":"x","catalog_revision":"x","steps":[{"after_ms":null,"changes":[]}]}`,
			`{"stream_id":"x","catalog_revision":"x","steps":[{"changes":[]}]}`,
			`{"stream_id":"x","catalog_revision":"x","steps":[{"after_ms":1.5,"changes":[]}]}`,
			`{"stream_id":"x","catalog_revision":"x","steps":[],"typo":1}`,
		} {
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(raw)})
			if err == nil && !result.IsError {
				t.Fatal("invalid arguments accepted", name, raw)
			}
		}
	}
}
