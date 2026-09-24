package svpub

import (
	"testing"

	"pbmt/internal/domain/sv"
)

func TestPublisherSnapshotDistinguishesUnpreparedWaveform(t *testing.T) {
	s := New(PublisherConfig{SmpRate: 80, SampleTimingFrequency: 50}, nil, nil, nil)
	if s.Snapshot().Ready {
		t.Fatal("unprepared waveform marked ready")
	}
	var settings [sv.NumChannels]ChannelSetting
	for i := range settings {
		settings[i] = ChannelSetting{RMS: 1, Frequency: 50, PhaseDeg: 30}
	}
	if err := s.SetWaveform(settings, true, sv.SmpSynchGlobal); err != nil {
		t.Fatal(err)
	}
	v := s.Snapshot()
	if !v.Ready || !v.Simulation || v.Synch != sv.SmpSynchGlobal || v.Settings != settings {
		t.Fatal("waveform snapshot inconsistent")
	}
	v.Settings[0].RMS = 999
	if s.Snapshot().Settings[0].RMS != 1 {
		t.Fatal("snapshot aliases waveform")
	}
}
