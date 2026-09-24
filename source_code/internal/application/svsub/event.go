package svsub

import (
	"time"

	"pbmt/internal/domain/sv"
)

// EventReason is the reason an SV diagnostic event was generated.
type EventReason string

const (
	ReasonFirstSeen      EventReason = "first_seen"       // first frame from the stream
	ReasonStreamLost     EventReason = "stream_lost"      // no frames received before the timeout
	ReasonStreamRestored EventReason = "stream_restored"  // stream returned after being lost
	ReasonSmpCntGap      EventReason = "smpcnt_gap"       // missing samples
	ReasonConfRevChanged EventReason = "conf_rev_changed" // confRev changed
	ReasonSyncChanged    EventReason = "sync_changed"     // smpSynch changed
	ReasonStats          EventReason = "stats"            // periodic statistics
	ReasonSnapshot       EventReason = "snapshot"         // live value snapshot for the GUI
)

// Event is an SV subscriber diagnostic event.
// It is published when stream state changes, not for every sample.
type Event struct {
	Reason       EventReason
	Subscription string
	ASDU         *sv.ASDU // nil for stream_lost and stats
	ReceivedAt   time.Time

	// Gap details:
	ExpectedSmpCnt uint16
	ActualSmpCnt   uint16
	MissedSamples  int

	// conf_rev_changed details:
	PrevConfRev uint32

	// sync_changed details:
	PrevSmpSynch sv.SmpSynch

	// stats details (periodic summary):
	Stats *StreamStats
}

// StreamStats contains accumulated stream statistics.
type StreamStats struct {
	SamplesReceived uint64
	GapEvents       uint64 // number of gap events, not samples
	MissedTotal     uint64 // total number of missed samples
	LastSmpCnt      uint16
	LastConfRev     uint32
	SmpSynch        sv.SmpSynch
	Simulation      bool
	SmpRate         uint16
	FrequencyHz     [sv.NumChannels]float64
	Instant         [sv.NumChannels]float64
	RMS             [sv.NumChannels]float64
	Angle           [sv.NumChannels]float64
	Quality         [sv.NumChannels]sv.Quality
}

// EventSink consumes SV diagnostic events.
type EventSink interface {
	OnSVEvent(Event)
}
