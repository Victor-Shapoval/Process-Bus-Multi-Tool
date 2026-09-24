package supervisor

import (
	"testing"

	"pbmt/internal/application/svpub"
	"pbmt/internal/config"
	"pbmt/internal/domain/sv"
)

func TestSVPublisherSnapshot(t *testing.T) {
	m := New(&config.Config{}, nil)
	if _, ok := m.SVPublisherSnapshot("stream"); ok {
		t.Fatal("snapshot exists before start")
	}
	svc := svpub.New(svpub.PublisherConfig{SmpRate: 80, SampleTimingFrequency: 50}, discardFrameSink{}, nil, nil)
	m.modules[ModuleSVPub] = &moduleRuntime{status: StatusRunning, svStreams: map[string]*svpub.Service{"stream": svc}}
	if _, ok := m.SVPublisherSnapshot("unknown"); ok {
		t.Fatal("unknown stream has a snapshot")
	}
	if snapshot, ok := m.SVPublisherSnapshot("stream"); !ok || snapshot.Ready {
		t.Fatal("unconfigured stream snapshot is incorrect")
	}
	var values [sv.NumChannels]svpub.ChannelSetting
	for ch := range values {
		values[ch].Frequency = 50
	}
	values[0] = svpub.ChannelSetting{RMS: 400, Frequency: 50}
	if err := svc.SetWaveform(values, false, sv.SmpSynchNone); err != nil {
		t.Fatal(err)
	}
	if snapshot, ok := m.SVPublisherSnapshot("stream"); !ok || !snapshot.Ready || snapshot.Settings != values {
		t.Fatal("configured stream snapshot is incorrect")
	}
}
