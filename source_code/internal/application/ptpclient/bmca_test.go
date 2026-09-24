package ptpclient

import (
	"testing"
	"time"

	"pbmt/internal/domain/ptp"
)

func TestForeignMasterQualificationUsesBoundedAnnounceWindow(t *testing.T) {
	var master ForeignMaster
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !master.recordAnnounce(start, 4*time.Second, 1) {
		t.Fatal("first Announce was rejected")
	}
	if !master.recordAnnounce(start.Add(5*time.Second), 4*time.Second, 2) {
		t.Fatal("second distinct Announce was rejected")
	}
	if master.AnnounceCount != 1 || master.qualified() {
		t.Fatalf("stale Announce qualified master: count=%d qualified=%t", master.AnnounceCount, master.qualified())
	}
	if !master.recordAnnounce(start.Add(6*time.Second), 4*time.Second, 3) {
		t.Fatal("third distinct Announce was rejected")
	}
	if master.AnnounceCount != foreignMasterThreshold || !master.qualified() {
		t.Fatalf("fresh Announces did not qualify master: count=%d qualified=%t", master.AnnounceCount, master.qualified())
	}
}

func makeDS(priority1 uint8, clockClass uint8, accuracy uint8, variance uint16, priority2 uint8, identity byte) Dataset {
	return Dataset{
		Priority1: priority1,
		Identity:  ptp.ClockIdentity{identity, 0, 0, 0, 0, 0, 0, 0},
		Quality: ptp.ClockQuality{
			ClockClass:              clockClass,
			ClockAccuracy:           accuracy,
			OffsetScaledLogVariance: variance,
		},
		Priority2: priority2,
	}
}

func TestDSCmpPriority1(t *testing.T) {
	a := makeDS(100, 6, 0x21, 0x49A0, 128, 1)
	b := makeDS(200, 6, 0x21, 0x49A0, 128, 2)
	if DSCmp(&a, &b) >= 0 {
		t.Fatal("expected a better (lower priority1)")
	}
}

func TestDSCmpClockClass(t *testing.T) {
	a := makeDS(128, 6, 0x21, 0x49A0, 128, 1)
	b := makeDS(128, 248, 0x21, 0x49A0, 128, 2)
	if DSCmp(&a, &b) >= 0 {
		t.Fatal("expected a better (lower clockClass)")
	}
}

func TestDSCmpAccuracy(t *testing.T) {
	a := makeDS(128, 6, 0x21, 0x49A0, 128, 1) // 100ns
	b := makeDS(128, 6, 0x25, 0x49A0, 128, 2) // 10us
	if DSCmp(&a, &b) >= 0 {
		t.Fatal("expected a better (better accuracy)")
	}
}

func TestDSCmpPriority2(t *testing.T) {
	a := makeDS(128, 6, 0x21, 0x49A0, 100, 1)
	b := makeDS(128, 6, 0x21, 0x49A0, 200, 2)
	if DSCmp(&a, &b) >= 0 {
		t.Fatal("expected a better (lower priority2)")
	}
}

func TestDSCmpIdentityTiebreak(t *testing.T) {
	a := makeDS(128, 6, 0x21, 0x49A0, 128, 1)
	b := makeDS(128, 6, 0x21, 0x49A0, 128, 2)
	if DSCmp(&a, &b) >= 0 {
		t.Fatal("expected a better (lower identity)")
	}
}

func TestDSCmpNilHandling(t *testing.T) {
	a := makeDS(128, 6, 0x21, 0x49A0, 128, 1)
	if DSCmp(&a, nil) >= 0 {
		t.Fatal("a should be better than nil")
	}
	if DSCmp(nil, &a) <= 0 {
		t.Fatal("nil should be worse than a")
	}
	if DSCmp(nil, nil) != 0 {
		t.Fatal("nil vs nil should be 0")
	}
}

func TestSelectBestMaster(t *testing.T) {
	receiver := ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentity{0x10, 0, 0, 0xFF, 0xFE, 0, 0, 0},
		PortNumber:    1,
	}

	m1 := &ForeignMaster{
		Identity: ptp.PortIdentity{
			ClockIdentity: ptp.ClockIdentity{1, 0, 0, 0xFF, 0xFE, 0, 0, 0},
			PortNumber:    1,
		},
		Announce: ptp.AnnounceBody{
			GrandmasterPriority1:    128,
			GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 248, ClockAccuracy: 0xFE, OffsetScaledLogVariance: 0xFFFF},
			GrandmasterPriority2:    128,
			GrandmasterIdentity:     ptp.ClockIdentity{1, 0, 0, 0xFF, 0xFE, 0, 0, 0},
		},
		Header: ptp.Header{
			SourcePortIdentity: ptp.PortIdentity{
				ClockIdentity: ptp.ClockIdentity{1, 0, 0, 0xFF, 0xFE, 0, 0, 0},
				PortNumber:    1,
			},
		},
	}

	m2 := &ForeignMaster{
		Identity: ptp.PortIdentity{
			ClockIdentity: ptp.ClockIdentity{2, 0, 0, 0xFF, 0xFE, 0, 0, 0},
			PortNumber:    1,
		},
		Announce: ptp.AnnounceBody{
			GrandmasterPriority1:    128,
			GrandmasterClockQuality: ptp.ClockQuality{ClockClass: 6, ClockAccuracy: 0x21, OffsetScaledLogVariance: 0x49A0},
			GrandmasterPriority2:    128,
			GrandmasterIdentity:     ptp.ClockIdentity{2, 0, 0, 0xFF, 0xFE, 0, 0, 0},
		},
		Header: ptp.Header{
			SourcePortIdentity: ptp.PortIdentity{
				ClockIdentity: ptp.ClockIdentity{2, 0, 0, 0xFF, 0xFE, 0, 0, 0},
				PortNumber:    1,
			},
		},
	}

	best := SelectBestMaster([]*ForeignMaster{m1, m2}, receiver)
	if best != m2 {
		t.Fatalf("expected m2 (class 6) to be best, got m1 (class 248)")
	}
}
