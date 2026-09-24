package goosesub

import (
	"time"

	"pbmt/internal/domain/goose"
)

// EventReason is the reason an event was generated.
type EventReason string

const (
	ReasonFirstSeen        EventReason = "first_seen"        // first frame from the stream
	ReasonStateChange      EventReason = "state_change"      // stNum increased (new event)
	ReasonPublisherRestart EventReason = "publisher_restart" // stNum decreased (publisher restart)
	ReasonConfRevChanged   EventReason = "conf_rev_changed"  // confRev changed (reconfiguration)
	ReasonStreamLost       EventReason = "stream_lost"       // TimeAllowedToLive exceeded
	ReasonStreamRestored   EventReason = "stream_restored"   // stream returned after being lost
)

// Event is a GOOSE subscriber domain event.
// It is published when the publisher state changes or the watchdog triggers.
type Event struct {
	// Reason is why the event was published.
	Reason EventReason

	// Subscription is the subscription name.
	Subscription string

	// PDU is the decoded message. For Reason=StreamLost, it contains the last
	// known PDU and may be nil if the stream was never observed.
	PDU *goose.PDU

	// ReceivedAt is the frame capture time, or the detection time for lost/restored events.
	ReceivedAt time.Time

	// Difference from the previous observation (set for Reason=StateChange):
	PreviousStNum uint32
	SqNumGap      int32 // >0 gap, <0 rollback; 0 is normal (a new stNum resets sqNum to 0)
}

// EventSink consumes GOOSE events.
type EventSink interface {
	OnGooseEvent(Event)
}
