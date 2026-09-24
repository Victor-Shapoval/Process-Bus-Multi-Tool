package goosepub

import (
	"testing"

	"pbmt/internal/domain/goose"
)

func TestPublisherSnapshotIsDetachedConfiguredState(t *testing.T) {
	s := New(PublisherConfig{InitialData: []goose.DataValue{{Type: goose.DataTypeStructure,
		Children: []goose.DataValue{goose.NewQualityBitString(1)}}}}, nil, nil, nil)
	v := s.Snapshot()
	if v.ChangedAt.IsZero() {
		t.Fatal("missing change time")
	}
	v.Data[0].Children[0].Bytes[0] = 0
	if s.Snapshot().Data[0].Children[0].Bytes[0] == 0 {
		t.Fatal("snapshot aliases publisher state")
	}
	s.ApplyState([]goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}, true, true)
	v = s.Snapshot()
	if !v.Test || !v.Simulation || !v.Data[0].Bool {
		t.Fatal("snapshot missed applied state")
	}
}
