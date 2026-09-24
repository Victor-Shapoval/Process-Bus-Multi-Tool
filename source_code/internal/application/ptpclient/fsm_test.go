package ptpclient

import "testing"

func TestFSMInitToListening(t *testing.T) {
	s := SlavePortFSM(StateInitializing, EventInitComplete)
	if s != StateListening {
		t.Fatalf("want LISTENING, got %s", s)
	}
}

func TestFSMListeningToUncalibrated(t *testing.T) {
	s := SlavePortFSM(StateListening, EventRSSlave)
	if s != StateUncalibrated {
		t.Fatalf("want UNCALIBRATED, got %s", s)
	}
}

func TestFSMUncalibratedToSlave(t *testing.T) {
	s := SlavePortFSM(StateUncalibrated, EventMasterClockSelected)
	if s != StateSlave {
		t.Fatalf("want SLAVE, got %s", s)
	}
}

func TestFSMSlaveAnnounceTimeout(t *testing.T) {
	s := SlavePortFSM(StateSlave, EventAnnounceTimeout)
	if s != StateListening {
		t.Fatalf("want LISTENING, got %s", s)
	}
}

func TestFSMSlaveSyncFault(t *testing.T) {
	s := SlavePortFSM(StateSlave, EventSyncFault)
	if s != StateUncalibrated {
		t.Fatalf("want UNCALIBRATED, got %s", s)
	}
}

func TestFSMSlaveMasterChanged(t *testing.T) {
	s := SlavePortFSM(StateSlave, EventMasterChanged)
	if s != StateUncalibrated {
		t.Fatalf("want UNCALIBRATED, got %s", s)
	}
}

func TestFSMFaultAndClear(t *testing.T) {
	s := SlavePortFSM(StateListening, EventFault)
	if s != StateFaulty {
		t.Fatalf("want FAULTY, got %s", s)
	}
	s = SlavePortFSM(s, EventFaultCleared)
	if s != StateInitializing {
		t.Fatalf("want INITIALIZING, got %s", s)
	}
}

func TestFSMListeningStaysOnTimeout(t *testing.T) {
	s := SlavePortFSM(StateListening, EventAnnounceTimeout)
	if s != StateListening {
		t.Fatalf("slave-only: want LISTENING on announce timeout, got %s", s)
	}
}

func TestFSMUnknownEventNoChange(t *testing.T) {
	s := SlavePortFSM(StateSlave, EventInitComplete) // irrelevant event
	if s != StateSlave {
		t.Fatalf("irrelevant event should not change state, got %s", s)
	}
}
