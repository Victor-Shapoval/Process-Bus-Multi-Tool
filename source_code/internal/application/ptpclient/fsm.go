package ptpclient

// PortState is a PTP port state (IEEE 1588-2008, Section 9.2.5).
type PortState int

const (
	StateInitializing PortState = iota
	StateListening
	StateUncalibrated
	StateSlave
	StateFaulty
	StateStopped
)

func (s PortState) String() string {
	switch s {
	case StateInitializing:
		return "INITIALIZING"
	case StateListening:
		return "LISTENING"
	case StateUncalibrated:
		return "UNCALIBRATED"
	case StateSlave:
		return "SLAVE"
	case StateFaulty:
		return "FAULTY"
	case StateStopped:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// FSMEvent is a port state-machine event.
type FSMEvent int

const (
	EventInitComplete        FSMEvent = iota
	EventRSSlave                      // BMCA selected a master: become a slave
	EventMasterClockSelected          // servo locked: transition to SLAVE
	EventAnnounceTimeout              // Announce receipt timeout: return to LISTENING
	EventSyncFault                    // synchronization failure: return to UNCALIBRATED
	EventFault                        // critical error: transition to FAULTY
	EventFaultCleared                 // fault cleared
	EventMasterChanged                // BMCA selected another master (mdiff)
)

// SlavePortFSM is a simplified FSM for a slave-only client.
// It omits MASTER, GRAND_MASTER, PASSIVE, and PRE_MASTER because the client is slave-only.
func SlavePortFSM(state PortState, event FSMEvent) PortState {
	switch state {
	case StateInitializing:
		switch event {
		case EventFault:
			return StateFaulty
		case EventInitComplete:
			return StateListening
		}

	case StateFaulty:
		if event == EventFaultCleared {
			return StateInitializing
		}

	case StateListening:
		switch event {
		case EventFault:
			return StateFaulty
		case EventRSSlave:
			return StateUncalibrated
		case EventAnnounceTimeout:
			return StateListening // slave-only: remain in LISTENING
		}

	case StateUncalibrated:
		switch event {
		case EventFault:
			return StateFaulty
		case EventAnnounceTimeout:
			return StateListening
		case EventMasterClockSelected:
			return StateSlave
		case EventMasterChanged:
			return StateUncalibrated // new master: recalibrate
		}

	case StateSlave:
		switch event {
		case EventFault:
			return StateFaulty
		case EventAnnounceTimeout:
			return StateListening
		case EventSyncFault:
			return StateUncalibrated
		case EventMasterChanged:
			return StateUncalibrated
		}
	}

	return state
}
