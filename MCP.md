# MCP

PBMT exposes GOOSE and Sampled Values (SV) monitoring and control through a Model Context Protocol (MCP) server. An MCP-compatible agent can read signal values, set current/voltage RMS and phase angles, change GOOSE DataSet values, run timed sequences, and measure an SV-to-GOOSE response.

Each project represents one physical protection and control IED. The engineer prepares the network and stream configuration, configures PTP if required, verifies communication with the IED, and starts the required modules before enabling MCP. The agent operates on these prepared GOOSE/SV streams; PTP Client and Server configuration and control remain exclusively with the engineer and are not exposed through MCP. While MCP is active, GOOSE/SV control belongs to the agent and Manual control is locked.

## Available Tools

- `get_status` — GOOSE/SV module states and errors, and the current control owner.
- `get_catalog` — configured streams and signals: IDs, types, units, directions, and catalog revision. No live values.
- `sv_set` — update current/voltage RMS and phase angles for selected channels of a running Publisher.
- `sv_get` — read Subscriber RMS, phase angles, frequency, quality, and data freshness, or current Publisher settings.
- `goose_set` — update selected DataSet values of a running Publisher, including binary signals and other supported types.
- `goose_get` — read the latest accepted GOOSE values, their freshness and ICD validation status, or current Publisher values.
- `t_set_goose` — change GOOSE values using a timed sequence.
- `t_set_sv` — change SV RMS and phase angles using a timed sequence.
- `get_sequence` — sequence state, applied step count, timing, lateness, and error.
- `cancel_sequence` — cancel remaining steps without stopping the Publisher; retain the last applied values.
- `run_test` — execute SV steps while waiting for a GOOSE edge, with built-in response-time measurement.
- `get_test` — test outcome, response step, applied signal settings, response time, and reset confirmation.
- `cancel_test` — cancel a test and reset the affected channel RMS values to zero inside PBMT.

A successful `set` does not confirm reception by the IED. The MCP tools themselves are listed through the standard `tools/list` method.

## Timed Sequences

- Input: `stream_id`, `catalog_revision`, `steps: [{after_ms, changes}]`. The `changes` format matches the corresponding `set` tool.
- `after_ms` is the interval from the previous **planned** step; for the first step, it is measured from sequence start. The first interval may be `0`; all subsequent intervals must be greater than `0`.
- All steps are validated before execution. The call returns a `sequence_id` immediately; execution runs inside PBMT using a monotonic clock, without hard real-time guarantees.
- `max_lateness_ms` is the allowed step application delay: default `100`, range `1–60000`. Exceeding this limit or accumulating overdue steps fails the sequence without a burst of catch-up commands.
- Maximum 1000 steps and 24 hours per sequence. Only one active sequence per stream; other writes to that stream are rejected. Different streams may run concurrently, without guaranteed simultaneous updates across streams.
- States: `running`, `completed`, `cancelled`, `failed`. Cancellation, completion, and failure retain the last applied values. Returning to the initial values requires an explicit final step.
- Stop MCP cancels sequences and returns control to Manual. Loss of the MCP connection cancels sequences and stops GOOSE/SV Publishers.
- `get_status` also returns sequence summaries. Up to 64 records are retained in memory until MCP stops; the oldest finished records are evicted. If a start response is lost, check `get_status` before retrying the start.

## Response-Time Tests

- One SV Publisher → one BOOLEAN signal from a GOOSE Subscriber. Combined GOOSE/SV stimuli and waiting for SV measurements are not implemented.
- Input: `stream_id`, `catalog_revision`, `steps: [{after_ms, changes}]`, `expect: {stream_id, signal_id, edge}`, `timeout_ms`, and optional `measure_from_step` and `max_lateness_ms`.
- Steps follow `t_set_sv`: RMS in A/V without CT-ratio scaling, and phase angles in degrees. The first step must have `after_ms: 0`; subsequent intervals are measured from the previous planned step. For example, a +100 A increase every 200 ms is expressed as individual steps. Every step must change settings and be scheduled before the timeout.
- `edge`: `rising` or `falling`. Requires a fresh, unambiguous, ICD-matched input whose initial value is opposite to the expected edge's target value. Test/Simulation/NdsCom flags and nonzero Quality values in the DataSet are rejected.
- Observation starts before the stimulus. Measurement begins immediately before the successful send of the selected step's first SV frame (`measure_from_step`, 1-based, default 1). It ends when the capture read returns the first accepted GOOSE frame carrying the expected edge, before internal queues. Intervals use a monotonic clock; send timestamps use the project clock. `response_at` is derived from the first frame's timestamp plus the monotonic interval.
- These are software, not hardware, timestamps. Driver/USB/capture delays are included; hard real-time is not guaranteed. Accepted short pulses are retained independently of GUI and MCP polling. Queue overflow, stream loss, source restart, or an ICD mismatch fails the test.
- `timeout_ms`: 1–60000 ms from the first frame; send confirmation is awaited for no more than 1 s. Only one test may be active; other commands cannot write to its output stream. The call returns a `test_id`; if the start outcome is unknown, check `get_status` before retrying.
- On an edge, timeout, cancellation, failure, or Stop MCP, the RMS of every channel mentioned in any step is reset to zero. Other channels, frequency, quality, and protocol settings are preserved. PBMT waits for confirmation that the reset frame was sent; if reset cannot be confirmed, it revokes control and stops both GOOSE/SV Publishers, not PTP.
- Results: `triggered`, `timeout`, `cancelled`, or `failed`; `elapsed_ms`, `from_start_ms`, `response_step`, sent steps with their settings, `reset_confirmed`, and `reset_from_start_ms`. Up to 64 results are retained in memory for the MCP server's lifetime; no packet capture is required. `get_status` includes test summaries.

## Limitations

- The MCP client must send the HTTP header `Authorization: Bearer <token>`. When configuring the header manually, the `Bearer ` prefix is required; if the client adds it automatically, enter only the token. PBMT's Token field contains the token without the prefix. `Authorization: <token>` without `Bearer` is not accepted.
