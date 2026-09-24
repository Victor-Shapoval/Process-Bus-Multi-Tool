package supervisor

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"pbmt/internal/application/ptpserver"
	"pbmt/internal/config"
	"pbmt/internal/domain/ptp"
)

func TestLocalPTPRequiresRunningServer(t *testing.T) {
	m := New(&config.Config{PTPClient: config.PTPClientCfg{Enabled: true, Source: "local"}}, nil)
	defer m.Close()
	if err := m.Start(ModulePTPClient); err == nil {
		t.Fatal("Local started without a server")
	}
	if m.clock.SourceStatus().PTPEnabled {
		t.Fatal("failed Local start enabled the clock")
	}
}

func TestLocalPTPBindsWithoutNetworkAndRequiresExplicitRestartAfterLoss(t *testing.T) {
	m := New(&config.Config{PTPClient: config.PTPClientCfg{Enabled: true, Source: "local", Interface: "does-not-exist"}}, nil)
	defer m.Close()
	var available atomic.Bool
	available.Store(true)
	server := ptpserver.New(nil, ptp.PortIdentity{PortNumber: 1}, ptpserver.Config{Profile: ptp.DefaultProfile, UTCOffset: 37}, nil)
	m.modules[ModulePTPServer] = &moduleRuntime{
		status: StatusRunning, ptpServer: server,
		ptpTime: func() (time.Time, error) {
			if !available.Load() {
				return time.Time{}, errors.New("server unavailable")
			}
			return time.Now().Add(-24 * time.Hour), nil
		},
	}
	if err := m.Start(ModulePTPClient); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := m.PTPClientSnapshot()
	if !ok || snapshot.Source != "local" || !snapshot.ClockStatus.Synchronized || snapshot.Transport != "internal" {
		t.Fatalf("incorrect local snapshot: %+v", snapshot)
	}
	available.Store(false)
	deadline := time.Now().Add(2 * time.Second)
	for m.Status(ModulePTPClient).State != StatusError && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Status(ModulePTPClient).State != StatusError {
		t.Fatal("source loss did not fail the client")
	}
	available.Store(true)
	snapshot, ok = m.PTPClientSnapshot()
	if !ok || snapshot.ClockStatus.Synchronized || !snapshot.ClockStatus.PTPEnabled {
		t.Fatalf("source loss did not stay in unsynchronized Local holdover: %+v", snapshot)
	}
	if err := m.Start(ModulePTPClient); err != nil {
		t.Fatal(err)
	}
	if !m.clock.SourceStatus().Synchronized {
		t.Fatal("explicit restart did not rebind Local")
	}
	if err := m.Stop(ModulePTPClient); err != nil {
		t.Fatal(err)
	}
	if m.clock.SourceStatus().PTPEnabled {
		t.Fatal("explicit stop did not disable the binding")
	}
}
