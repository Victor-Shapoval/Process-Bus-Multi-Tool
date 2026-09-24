package supervisor

import (
	"reflect"
	"sync"
	"testing"

	"pbmt/internal/application/control"
	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/svpub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
)

func TestAgentReleasePreservesOutputsAndFencesOldLease(t *testing.T) {
	m := New(&config.Config{}, nil)
	defer m.Close()
	svc := svpub.New(svpub.PublisherConfig{SmpRate: 80, SampleTimingFrequency: 50}, discardFrameSink{}, nil, nil)
	var values [sv.NumChannels]svpub.ChannelSetting
	for ch := range values {
		values[ch] = svpub.ChannelSetting{RMS: 15, PhaseDeg: float64(ch) * 30, Frequency: 50}
	}
	if err := svc.SetWaveform(values, true, sv.SmpSynchGlobal); err != nil {
		t.Fatal(err)
	}
	for _, id := range control.Modules() {
		m.modules[id] = &moduleRuntime{status: StatusRunning}
	}
	m.modules[ModuleSVPub].svStreams = map[string]*svpub.Service{"stream": svc}
	gooseService := goosepub.New(goosepub.PublisherConfig{}, discardFrameSink{}, nil, nil)
	gooseService.ApplyState([]goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}, true, true)
	m.modules[ModuleGoosePub].goosePubs = map[string]*goosepub.Service{"goose": gooseService}
	gooseBefore := gooseService.Snapshot()
	a, err := m.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	before := svc.Snapshot()
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if m.AgentActive() || svc.Snapshot() != before {
		t.Fatal("handover changed outputs")
	}
	if current, ok := m.GoosePublisherSnapshot("goose"); !ok || !reflect.DeepEqual(current, gooseBefore) {
		t.Fatal("handover changed GOOSE output")
	}
	for _, id := range control.Modules() {
		if m.Status(id).State != StatusRunning {
			t.Fatal("handover stopped module", id)
		}
	}
	if err := a.ApplySV("stream", values, false, sv.SmpSynchNone); err == nil {
		t.Fatal("old lease can write")
	}
	if _, err := a.Statuses(); err == nil {
		t.Fatal("old lease can read")
	}
	if err := m.SetSVPublisherWaveform("stream", values, true, sv.SmpSynchGlobal); err != nil {
		t.Fatal("manual not restored", err)
	}
	b, err := m.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil || !m.AgentActive() {
		t.Fatal("old lease stopped new owner")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Status(ModuleSVPub).State != StatusStopped {
		t.Fatal("fault shutdown stopped working")
	}
}

func TestAgentLeaseExcludesManualAndStopsOnlyPublishers(t *testing.T) {
	cfg := &config.Config{}
	m := New(cfg, nil)
	defer m.Close()
	for _, id := range []ModuleID{ModuleGooseSub, ModuleGoosePub, ModuleSVSub, ModuleSVPub, ModulePTPClient, ModulePTPServer} {
		m.modules[id] = &moduleRuntime{status: StatusRunning}
	}
	a, err := m.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	if !m.AgentActive() {
		t.Fatal("ownership was not acquired")
	}
	if _, err := m.AcquireAgent(); err == nil {
		t.Fatal("two owners accepted")
	}
	for _, id := range []ModuleID{ModuleGoosePub, ModuleSVPub, ModulePTPClient, ModulePTPServer} {
		if m.Start(id) == nil || m.Stop(id) == nil {
			t.Fatal("manual lifecycle was not blocked", id)
		}
	}
	if _, err := m.ApplyGoosePublisherState("test", nil, false, false); err == nil {
		t.Fatal("manual GOOSE write accepted")
	}
	if m.SetSVPublisherWaveform("test", [sv.NumChannels]svpub.ChannelSetting{}, false, 0) == nil {
		t.Fatal("manual SV write accepted")
	}
	m.UpdateConfig(&config.Config{})
	if m.cfg != cfg {
		t.Fatal("configuration changed during MCP ownership")
	}
	states, err := a.Statuses()
	if err != nil || len(states) != 4 {
		t.Fatalf("states: %v %v", states, err)
	}
	for _, st := range states {
		if st.ID == ModulePTPClient || st.ID == ModulePTPServer {
			t.Fatal("PTP exposed")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = a.Statuses()
			}
		}()
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if m.AgentActive() {
		t.Fatal("ownership not released")
	}
	for _, id := range []ModuleID{ModuleGoosePub, ModuleSVPub} {
		if m.Status(id).State != StatusStopped {
			t.Fatal("publisher still running", id)
		}
	}
	for _, id := range []ModuleID{ModuleGooseSub, ModuleSVSub, ModulePTPClient, ModulePTPServer} {
		if m.Status(id).State != StatusRunning {
			t.Fatal("receiver/PTP changed", id)
		}
	}
	second, err := m.AcquireAgent()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil || !m.AgentActive() {
		t.Fatal("old lease closed new owner")
	}
	if _, err := a.Statuses(); err == nil {
		t.Fatal("old lease can read new session")
	}
	if _, err := a.ApplyGoose("test", nil, false, false); err == nil {
		t.Fatal("old lease can write")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Statuses(); err == nil {
		t.Fatal("closed project lease remains valid")
	}
	if _, err := m.AcquireAgent(); err == nil {
		t.Fatal("closed project acquired")
	}
}
