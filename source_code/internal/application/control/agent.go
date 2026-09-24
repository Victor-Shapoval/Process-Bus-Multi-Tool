package control

import (
	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/svpub"
	"pbmt/internal/application/svsub"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

// AgentProvider owns exclusive control of the current project. Manual commands
// are rejected at the application boundary, not merely hidden in the GUI.
type AgentProvider interface {
	AcquireAgent() (Agent, error)
	AgentActive() bool
}

// Agent deliberately has no PTP, module-start, configuration, or file APIs.
// Close revokes its authority and stops only the two publisher modules.
type Agent interface {
	Statuses() ([]ModuleStatus, error)
	GooseInput(string) ([]goosesub.Snapshot, ModuleStatus, error)
	SVInput(string) ([]svsub.Snapshot, ModuleStatus, error)
	GooseOutput(string) (goosepub.Snapshot, ModuleStatus, error)
	SVOutput(string) (svpub.Snapshot, ModuleStatus, error)
	ApplyGoose(string, []goose.DataValue, bool, bool) (bool, error)
	ApplySV(string, [sv.NumChannels]svpub.ChannelSetting, bool, sv.SmpSynch) error
	// Release returns control to Manual without stopping or changing outputs.
	Release() error
	Close() error
}

// TestAgent supplies loss-detecting input observation and exact waveform-send
// receipts. These are intentionally separate from best-effort GUI telemetry.
type TestAgent interface {
	ObserveGoose(string) (*goosesub.Watch, error)
	ApplySVTracked(string, [sv.NumChannels]svpub.ChannelSetting, bool, sv.SmpSynch) (<-chan svpub.SendReceipt, error)
}
