// Package control defines the application-facing API used by the GUI to
// supervise protocol modules. Concrete adapter wiring lives in supervisor.
package control

import (
	"time"

	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/ptpclient"
	"pbmt/internal/application/ptpserver"
	"pbmt/internal/application/svpub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

type ModuleID string

const (
	ModuleGooseSub  ModuleID = "goose_sub"
	ModuleGoosePub  ModuleID = "goose_pub"
	ModuleSVSub     ModuleID = "sv_sub"
	ModuleSVPub     ModuleID = "sv_pub"
	ModulePTPClient ModuleID = "ptp_client"
	ModulePTPServer ModuleID = "ptp_server"
)

func Modules() []ModuleID {
	return []ModuleID{
		ModuleGooseSub,
		ModuleGoosePub,
		ModuleSVSub,
		ModuleSVPub,
		ModulePTPClient,
		ModulePTPServer,
	}
}

type Status string

const (
	StatusStopped Status = "Stopped"
	StatusRunning Status = "Running"
	StatusError   Status = "Error"
)

type ModuleStatus struct {
	ID    ModuleID `json:"id"`
	State Status   `json:"state"`
	Error string   `json:"error,omitempty"`
}

// Event is best-effort operational telemetry for presentation clients. A slow
// client may miss events; protocol processing must never block on the GUI.
type Event struct {
	Module       ModuleID
	Time         time.Time
	Message      string
	Severity     string
	Subscription string
	GoosePDU     *goose.PDU
	SVValues     [sv.NumChannels]float64
	SVRMS        [sv.NumChannels]float64
	SVAngle      [sv.NumChannels]float64
	SVQuality    [sv.NumChannels]sv.Quality
	SVFrequency  [sv.NumChannels]float64
	SVSynch      sv.SmpSynch
	SVSimulation bool
	HasSVValues  bool
}

// Controller is the GUI-facing use-case boundary. Implementations own module
// lifecycles and the shared application clock, but never modify system time.
type Controller interface {
	UpdateConfig(*config.Config)
	Status(ModuleID) ModuleStatus
	Start(ModuleID) error
	Stop(ModuleID) error
	Close() error
	Subscribe(buffer int) (<-chan Event, func())

	ApplyGoosePublisherState(name string, data []goose.DataValue, test, simulation bool) (bool, error)
	SetSVPublisherWaveform(name string, settings [sv.NumChannels]svpub.ChannelSetting, simulation bool, smpSynch sv.SmpSynch) error
	SVPublisherSnapshot(name string) (svpub.Snapshot, bool)
	GoosePublisherSnapshot(name string) (goosepub.Snapshot, bool)
	GooseSubscriberSnapshots(name string) ([]goosesub.Snapshot, ModuleStatus)
	PTPClientSnapshot() (ptpclient.Snapshot, bool)
	PTPServerSnapshot() (ptpserver.Snapshot, bool)
}
