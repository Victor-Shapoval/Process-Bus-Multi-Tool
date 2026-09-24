package goosesub

import (
	"testing"
	"time"

	"pbmt/internal/domain/ethernet"
	"pbmt/internal/domain/goose"
)

func TestSnapshotTracksHeartbeatsWithoutGUIAndOwnsItsData(t *testing.T) {
	sub := testSub()
	svc := New(newFakeSource(), []*goose.Subscription{sub}, &collectSink{}, nil)
	now := time.Now()
	first := makePDU(1, 0, 1)
	svc.handleMatch(sub, first, ethernet.Frame{Timestamp: now})
	beat := makePDU(1, 1, 1)
	beat.AllData = []goose.DataValue{{Type: goose.DataTypeStructure, Children: []goose.DataValue{{Type: goose.DataTypeOctetString, Bytes: []byte{1, 2}}}}}
	beat.VLAN = &ethernet.VLANTag{VID: 1}
	svc.handleMatch(sub, beat, ethernet.Frame{Timestamp: now.Add(150 * time.Millisecond)})
	v := svc.Snapshots(sub.Name, now.Add(300*time.Millisecond))
	if len(v) != 1 || v[0].Stale || !v[0].ReceivedAt.Equal(now.Add(150*time.Millisecond)) {
		t.Fatalf("heartbeat missing: %+v", v)
	}
	v[0].PDU.AllData[0].Children[0].Bytes[0] = 9
	v[0].PDU.DstMAC[0] = 255
	v[0].PDU.VLAN.VID = 99
	again := svc.Snapshots(sub.Name, now.Add(time.Second))[0]
	if !again.Stale || again.PDU.AllData[0].Children[0].Bytes[0] != 1 || again.PDU.DstMAC[0] == 255 || again.PDU.VLAN.VID != 1 {
		t.Fatal("snapshot aliases receiver or misses expiry")
	}
	if len(svc.Snapshots("unknown", now)) != 0 {
		t.Fatal("unknown subscription matched")
	}
}
