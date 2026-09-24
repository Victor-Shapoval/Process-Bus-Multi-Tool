package svsub

import (
	"testing"
	"time"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/sv"
)

func TestSnapshotReadsCacheWithoutAdvancingEstimatorAndResetsOnGap(t *testing.T) {
	sub := testSub()
	svc := New(newFakeSource(), []*sv.Subscription{sub}, &collectSink{}, nil)
	now := time.Now()
	for i := 0; i < 80; i++ {
		a := makeASDU(uint16(i), 1, sv.SmpSynchGlobal)
		a.Channels[0] = 1000
		svc.handleMatch(sub, a, ethernet.Frame{Timestamp: now.Add(time.Duration(i) * 250 * time.Microsecond)})
	}
	var state *streamState
	for _, st := range svc.streams {
		state = st
	}
	state.snapshotStats()
	before := state.angleStability
	for i := 0; i < 20; i++ {
		v := svc.Snapshots(sub.Name, now.Add(30*time.Millisecond))
		if len(v) != 1 || v[0].Stale || !v[0].RMSReady || v[0].Stats == nil {
			t.Fatalf("snapshot: %+v", v)
		}
		v[0].Stats.RMS[0] = 999
	}
	if before != state.angleStability || state.latestStats.RMS[0] == 999 {
		t.Fatal("read advanced estimator or aliased cache")
	}
	if !svc.Snapshots(sub.Name, now.Add(time.Second))[0].Stale {
		t.Fatal("stale SV returned as fresh")
	}
	svc.handleMatch(sub, makeASDU(90, 1, sv.SmpSynchGlobal), ethernet.Frame{Timestamp: now.Add(31 * time.Millisecond)})
	v := svc.Snapshots(sub.Name, now.Add(32*time.Millisecond))[0]
	if v.RMSReady || v.Stats != nil {
		t.Fatal("gap retained old measurements")
	}
}
