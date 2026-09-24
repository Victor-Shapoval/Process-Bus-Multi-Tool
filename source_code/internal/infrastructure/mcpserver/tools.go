package mcpserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"pbmt/internal/application/automation"
)

const gooseChangesSchema = `{"type":"array","minItems":1,"items":{"type":"object","properties":{"signal_id":{"type":"string"},"value":{"type":["boolean","integer","number","string"]}},"required":["signal_id","value"],"additionalProperties":false}}`
const svChangesSchema = `{"type":"array","minItems":1,"maxItems":8,"items":{"type":"object","properties":{"signal_id":{"type":"string"},"rms":{"type":"number","minimum":0},"phase_deg":{"type":"number"}},"required":["signal_id"],"additionalProperties":false}}`

func (s *Server) addTools() {
	addTool(s, "get_catalog", "Get the configured GOOSE/SV streams, signal IDs, types, units, directions and catalog revision. No live values or PTP controls.", `{"type":"object","additionalProperties":false}`, true, func(_ struct{}) (any, error) { return s.api.GetCatalog() })
	addTool(s, "get_status", "Get current GOOSE/SV module states, errors and retained sequence/test summaries. Check sequences/tests before retrying a start with an uncertain response. Full test steps/settings are in get_test. A running subscriber does not imply fresh received data; use goose_get/sv_get for freshness.", `{"type":"object","additionalProperties":false}`, true, func(_ struct{}) (any, error) { return s.api.GetStatus() })
	const get = `{"type":"object","properties":{"stream_id":{"type":"string","description":"Exact stream ID from get_catalog"}},"required":["stream_id"],"additionalProperties":false}`
	addTool(s, "goose_get", "Read latest accepted GOOSE values, freshness and ICD validation, or current configured publisher values. Missing data is not false/zero. Output values do not confirm terminal reception.", get, true, func(r automation.StreamRequest) (any, error) { return s.api.GooseGet(r) })
	addTool(s, "sv_get", "Read SV RMS in A/V, phase in degrees relative to the configured base vector, frequency, quality and freshness. Null means unavailable. Subscriber A/V are wire-scaled without KI/KU; publisher RMS is secondary A/V.", get, true, func(r automation.StreamRequest) (any, error) { return s.api.SVGet(r) })
	addTool(s, "goose_set", "Atomically update selected entries of one running GOOSE publisher. Other entries and test/simulation flags are preserved. Values match catalog types: bool, signed/unsigned integer, float, string, RFC3339 UTC time, or integer Quality bitmask. Requires current catalog revision. Rejected while a sequence controls this stream.", setSchema(gooseChangesSchema), false, func(r automation.GooseSetRequest) (any, error) { return s.api.GooseSet(r) })
	addTool(s, "sv_set", "Atomically update selected channels of one running SV publisher. rms is secondary A/V (not peak); phase_deg is electrical generator phase. Omitted channels/fields, frequency, quality and protocol settings remain unchanged. No automatic module start. Rejected while a sequence controls this stream.", setSchema(svChangesSchema), false, func(r automation.SVSetRequest) (any, error) { return s.api.SVSet(r) })
	const timed = " Validate all steps then start an asynchronous sequence on one running publisher; returns sequence_id, not completion. after_ms is the interval from the previous PLANNED step (first: from sequence start); first may be zero, others must be positive. Total <=24h. Deadlines use server monotonic time, not hard real-time or packet transmission timestamps. Default max_lateness_ms=100; overdue/backlogged steps fail without catch-up bursts. This stream rejects other writes until finished/cancelled. Final, cancelled or failed sequences retain last applied values. Use get_sequence for outcome."
	addTool(s, "t_set_goose", "Schedule GOOSE DataSet changes; same values and preserved flags as goose_set."+timed, timedSchema(gooseChangesSchema), false, func(r automation.TimedGooseRequest) (any, error) { return s.api.TimedGooseSet(r) })
	addTool(s, "t_set_sv", "Schedule SV RMS (secondary A/V) and generator phase changes; frequency, quality and protocol settings remain unchanged."+timed, timedSchema(svChangesSchema), false, func(r automation.TimedSVRequest) (any, error) { return s.api.TimedSVSet(r) })
	const sequence = `{"type":"object","properties":{"sequence_id":{"type":"string"}},"required":["sequence_id"],"additionalProperties":false}`
	addTool(s, "get_sequence", "Get sequence state (running/completed/cancelled/failed), applied step count, timestamps, maximum observed application lateness and error. Up to 64 records retained in memory for this MCP server lifetime; oldest finished records are evicted. Not proof of terminal reception.", sequence, true, func(r automation.SequenceRequest) (any, error) { return s.api.GetSequence(r) })
	addTool(s, "cancel_sequence", "Cancel remaining steps and wait for any in-flight write. Keeps last applied values and publishers running. Already finished sequences are returned unchanged. Does not stop MCP or affect other streams.", sequence, false, func(r automation.SequenceRequest) (any, error) { return s.api.CancelSequence(r) })
	addTool(s, "run_test", "Run an asynchronous SV-step-to-GOOSE-edge test; returns test_id. One test at a time. Steps use secondary RMS A/V and generator phase; after_ms is cumulative planned spacing, first must be 0. Requires one fresh ICD-matched BOOLEAN input initially opposite the expected rising/falling edge. Observe before stimulus; time from the first successfully sent SV frame of measure_from_step (1-based, default 1) to application receipt of the first matching GOOSE edge, using the monotonic clock. Software boundaries, NOT NIC/wire timestamps. timeout_ms (1..60000) starts at first frame; all steps must precede timeout. At response, timeout, cancellation, or failure, PBMT zeros RMS of ALL channels mentioned by ANY step and confirms a reset frame; other channels, frequency and quality are preserved. Reset failure revokes control and stops both publishers. Stop MCP also resets active tests before Manual handover. No raw capture required. Use get_test/cancel_test and inspect get_status tests before retrying an uncertain start.", testSchema(), false, func(r automation.TestRequest) (any, error) { return s.api.RunTest(r) })
	const testID = `{"type":"object","properties":{"test_id":{"type":"string"}},"required":["test_id"],"additionalProperties":false}`
	addTool(s, "get_test", "Get retained test outcome, sent steps/settings, response step, software elapsed time and reset confirmation. Up to 64 test records retained for this MCP server lifetime. Short accepted pulses are retained independently of GUI/MCP polling.", testID, true, func(r automation.TestID) (any, error) { return s.api.GetTest(r) })
	addTool(s, "cancel_test", "Cancel a running test AND zero RMS of its affected channels inside PBMT; waits for reset confirmation. Unlike cancel_sequence, this does not leave the test stimulus applied. Already finished tests remain unchanged.", testID, false, func(r automation.TestID) (any, error) { return s.api.CancelTest(r) })
}

func testSchema() string {
	return `{"type":"object","properties":{"stream_id":{"type":"string"},"catalog_revision":{"type":"string"},"timeout_ms":{"type":"integer","minimum":1,"maximum":60000},"measure_from_step":{"type":"integer","minimum":1,"default":1},"max_lateness_ms":{"type":"integer","minimum":1,"maximum":60000,"default":100},"steps":{"type":"array","minItems":1,"maxItems":1000,"items":{"type":"object","properties":{"after_ms":{"type":"integer","minimum":0,"maximum":60000},"changes":` + svChangesSchema + `},"required":["after_ms","changes"],"additionalProperties":false}},"expect":{"type":"object","properties":{"stream_id":{"type":"string"},"signal_id":{"type":"string"},"edge":{"type":"string","enum":["rising","falling"]}},"required":["stream_id","signal_id","edge"],"additionalProperties":false}},"required":["stream_id","catalog_revision","steps","expect","timeout_ms"],"additionalProperties":false}`
}

func setSchema(changes string) string {
	return `{"type":"object","properties":{"stream_id":{"type":"string"},"catalog_revision":{"type":"string"},"changes":` + changes + `},"required":["stream_id","catalog_revision","changes"],"additionalProperties":false}`
}

func timedSchema(changes string) string {
	return `{"type":"object","properties":{"stream_id":{"type":"string"},"catalog_revision":{"type":"string"},"max_lateness_ms":{"type":"integer","minimum":1,"maximum":60000,"default":100},"steps":{"type":"array","minItems":1,"maxItems":1000,"items":{"type":"object","properties":{"after_ms":{"type":"integer","minimum":0,"maximum":86400000},"changes":` + changes + `},"required":["after_ms","changes"],"additionalProperties":false}}},"required":["stream_id","catalog_revision","steps"],"additionalProperties":false}`
}

func addTool[T any](s *Server, name, description, schema string, readOnly bool, call func(T) (any, error)) {
	s.rpc.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: json.RawMessage(schema),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input T
		var result any
		err := ctx.Err()
		if err == nil && !s.claim(req.Session) {
			err = errors.New("MCP session does not own this project")
		}
		if err == nil {
			err = automation.Decode(req.Params.Arguments, &input)
		}
		if err == nil {
			result, err = call(input)
		}
		if err != nil {
			// Never log arguments or values. Errors from operation validation are
			// deliberately phrased without echoing supplied signal payloads.
			s.log.Warn("MCP tool rejected", "tool", name, "error", err)
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, errors.New("cannot serialize tool result")
		}
		if !readOnly {
			s.log.Info("MCP command applied", "tool", name)
		}
		return &mcp.CallToolResult{StructuredContent: result, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
	})
}
