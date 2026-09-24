package ptpclient

import (
	"testing"
	"time"
)

func TestOffsetE2E(t *testing.T) {
	var p TsProc

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// Simulation: delay = 10 ms, offset = +5 ms.
	// t1 = 0, t2 = 15ms (offset+delay), t3 = 20ms, t4 = 30ms (delay)
	p.SetT1(base)
	p.SetT2(base.Add(15 * time.Millisecond))
	p.SetT3(base.Add(20 * time.Millisecond))
	p.SetT4(base.Add(30 * time.Millisecond))

	offset, delay, ok := p.OffsetE2E()
	if !ok {
		t.Fatal("OffsetE2E returned not ok")
	}

	// offset = ((t2-t1) - (t4-t3)) / 2 = (15ms - 10ms) / 2 = 2.5ms
	// delay = ((t2-t1) + (t4-t3)) / 2 = (15ms + 10ms) / 2 = 12.5ms
	wantOffset := 2500 * time.Microsecond
	wantDelay := 12500 * time.Microsecond

	if offset != wantOffset {
		t.Errorf("offset: want %v, got %v", wantOffset, offset)
	}
	if delay != wantDelay {
		t.Errorf("delay: want %v, got %v", wantDelay, delay)
	}
}

func TestOffsetE2EIncomplete(t *testing.T) {
	var p TsProc
	p.SetT1(time.Now())
	p.SetT2(time.Now())
	// t3, t4 not set
	_, _, ok := p.OffsetE2E()
	if ok {
		t.Fatal("expected not ok with missing timestamps")
	}
}

func TestOffsetE2EAppliesNonZeroCorrectionsToBothResults(t *testing.T) {
	var p TsProc
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	p.SetT1(base)
	p.SetT2(base.Add(15 * time.Millisecond))
	p.SetT3(base.Add(20 * time.Millisecond))
	p.SetT4(base.Add(30 * time.Millisecond))
	p.SetCorrectionSync(int64(time.Millisecond) << 16)
	p.SetCorrectionFollowUp(int64(time.Millisecond) << 16)
	p.SetCorrectionDelayResp(int64(4*time.Millisecond) << 16)

	offset, delay, ok := p.OffsetE2E()
	if !ok {
		t.Fatal("OffsetE2E returned not ok")
	}
	if want := 3500 * time.Microsecond; offset != want {
		t.Fatalf("offset: want %v, got %v", want, offset)
	}
	if want := 9500 * time.Microsecond; delay != want {
		t.Fatalf("delay: want %v, got %v", want, delay)
	}
}

func TestOffsetP2P(t *testing.T) {
	var p TsProc

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	p.SetT1(base)
	p.SetT2(base.Add(15 * time.Millisecond))
	p.SetPeerDelay(10 * time.Millisecond)

	offset, ok := p.OffsetP2P()
	if !ok {
		t.Fatal("OffsetP2P returned not ok")
	}

	// offset = (t2-t1) - peerDelay = 15ms - 10ms = 5ms
	wantOffset := 5 * time.Millisecond
	if offset != wantOffset {
		t.Errorf("offset: want %v, got %v", wantOffset, offset)
	}
}

func TestOffsetP2PAppliesNonZeroSyncCorrections(t *testing.T) {
	var p TsProc
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	p.SetT1(base)
	p.SetT2(base.Add(15 * time.Millisecond))
	p.SetPeerDelay(10 * time.Millisecond)
	p.SetCorrectionSync(int64(time.Millisecond) << 16)
	p.SetCorrectionFollowUp(int64(time.Millisecond) << 16)

	offset, ok := p.OffsetP2P()
	if !ok {
		t.Fatal("OffsetP2P returned not ok")
	}
	if want := 3 * time.Millisecond; offset != want {
		t.Fatalf("offset: want %v, got %v", want, offset)
	}
}

func TestTsProcReset(t *testing.T) {
	var p TsProc
	p.SetT1(time.Now())
	p.SetT2(time.Now())
	p.SetT3(time.Now())
	p.SetT4(time.Now())
	p.Reset()

	_, _, ok := p.OffsetE2E()
	if ok {
		t.Fatal("expected not ok after reset")
	}
}
