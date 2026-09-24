package supervisor

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"pbmt/internal/application/control"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/application/svsub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
	"pbmt/internal/domain/sv"
	"pbmt/internal/infrastructure/logging"
)

type lockedLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func loggingManager(level string) (*Manager, *lockedLogBuffer) {
	output := &lockedLogBuffer{}
	log, _ := logging.New(output, level)
	return New(&config.Config{}, log), output
}

func TestEveryModuleLogsLifecycleAndStartFailures(t *testing.T) {
	for _, id := range control.Modules() {
		t.Run(string(id), func(t *testing.T) {
			m, out := loggingManager(logging.LevelInfo)
			m.startModule = func(ModuleID, *config.Config) (*moduleRuntime, error) {
				return &moduleRuntime{status: StatusRunning}, nil
			}
			for i := 0; i < 2; i++ {
				if err := m.Start(id); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := m.Stop(id); err != nil {
					t.Fatal(err)
				}
			}
			text := out.String()
			if strings.Count(text, `msg="module started"`) != 1 || strings.Count(text, `msg="module stopped"`) != 1 || !strings.Contains(text, "module="+string(id)) {
				t.Fatalf("lifecycle log is missing/duplicated: %s", text)
			}
			m.startModule = func(ModuleID, *config.Config) (*moduleRuntime, error) {
				return nil, errors.New("interface unavailable")
			}
			if err := m.Start(id); err == nil {
				t.Fatal("failed start succeeded")
			}
			if !strings.Contains(out.String(), `level=ERROR msg="module start failed"`) || !strings.Contains(out.String(), "interface unavailable") {
				t.Fatalf("startup failure missing: %s", out.String())
			}
		})
	}
}

func TestRuntimeFailureIsLoggedOnceWithoutGUI(t *testing.T) {
	m, out := loggingManager(logging.LevelError)
	rt := &moduleRuntime{status: StatusRunning}
	m.modules[ModuleSVSub] = rt
	err := errors.New("capture failed")
	m.markRuntimeExit(ModuleSVSub, rt, err)
	m.markRuntimeExit(ModuleSVSub, rt, err)
	if err := m.Stop(ModuleSVSub); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); strings.Count(text, `msg="module failed"`) != 1 || !strings.Contains(text, "capture failed") || strings.Contains(text, "level=INFO") {
		t.Fatalf("unexpected failure log: %s", text)
	}
}

func TestConfigUpdateAndStopErrorsAreLogged(t *testing.T) {
	m, out := loggingManager(logging.LevelInfo)
	m.modules[ModuleGooseSub] = &moduleRuntime{status: StatusRunning}
	m.modules[ModuleSVSub] = &moduleRuntime{status: StatusRunning, close: func() error { return errors.New("close capture failed") }}
	m.UpdateConfig(&config.Config{})
	text := out.String()
	if !strings.Contains(text, `msg="module stopped" module=goose_sub`) || !strings.Contains(text, `msg="module stop failed" module=sv_sub`) || !strings.Contains(text, "reason=config_update") {
		t.Fatalf("config update stops missing: %s", text)
	}
	m.modules[ModuleGoosePub] = &moduleRuntime{status: StatusRunning, close: func() error { return errors.New("close injector failed") }}
	if err := m.Stop(ModuleGoosePub); err == nil || !strings.Contains(out.String(), "close injector failed") {
		t.Fatalf("stop error missing: %s", out.String())
	}
}

func TestStreamDiagnosticsAreLoggedWithoutSignalValuesOrGUI(t *testing.T) {
	for _, subscribe := range []bool{false, true} {
		m, out := loggingManager(logging.LevelDebug)
		if subscribe {
			_, cancel := m.Subscribe(1) // Never drain: a full GUI queue must not affect the log.
			defer cancel()
		}
		pdu := &goose.PDU{AllData: []goose.DataValue{{Type: goose.DataTypeVisibleString, String: "DO_NOT_LOG_SIGNAL"}}}
		gs := gooseEventSink{manager: m}
		for _, reason := range []goosesub.EventReason{goosesub.ReasonFirstSeen, goosesub.ReasonStateChange, goosesub.ReasonPublisherRestart, goosesub.ReasonConfRevChanged, goosesub.ReasonStreamLost, goosesub.ReasonStreamRestored} {
			gs.OnGooseEvent(goosesub.Event{Reason: reason, Subscription: "goose1", PDU: pdu, ReceivedAt: time.Now()})
		}
		ss := svEventSink{manager: m}
		stats := &svsub.StreamStats{RMS: [sv.NumChannels]float64{987654.125}, Instant: [sv.NumChannels]float64{987654.125}}
		for _, reason := range []svsub.EventReason{svsub.ReasonFirstSeen, svsub.ReasonStreamLost, svsub.ReasonStreamRestored, svsub.ReasonSmpCntGap, svsub.ReasonConfRevChanged, svsub.ReasonSyncChanged, svsub.ReasonSnapshot, svsub.ReasonStats} {
			ss.OnSVEvent(svsub.Event{Reason: reason, Subscription: "sv1", Stats: stats, ReceivedAt: time.Now()})
		}
		text := out.String()
		for _, event := range []string{"first_seen", "stream_lost", "stream_restored", "smpcnt_gap", "conf_rev_changed", "sync_changed", "publisher_restart"} {
			if !strings.Contains(text, "event="+event) {
				t.Errorf("missing %s: %s", event, text)
			}
		}
		for _, forbidden := range []string{"DO_NOT_LOG_SIGNAL", "987654.125", "event=snapshot", "event=stats", "AllData", "SVRMS"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("unexpected measurement in log: %s", forbidden)
			}
		}
	}
}

func TestStreamLevelsDistinguishLossFromModuleFailure(t *testing.T) {
	m, out := loggingManager(logging.LevelWarn)
	sink := gooseEventSink{manager: m}
	sink.OnGooseEvent(goosesub.Event{Reason: goosesub.ReasonStateChange, Subscription: "g"})
	sink.OnGooseEvent(goosesub.Event{Reason: goosesub.ReasonStreamRestored, Subscription: "g"})
	sink.OnGooseEvent(goosesub.Event{Reason: goosesub.ReasonStreamLost, Subscription: "g"})
	text := out.String()
	if !strings.Contains(text, "level=WARN") || !strings.Contains(text, "event=stream_lost") || strings.Contains(text, "event=state_change") || strings.Contains(text, "event=stream_restored") {
		t.Fatalf("unexpected diagnostic levels: %s", text)
	}
}

type cleanupBlockingLog struct {
	entered chan struct{}
	release chan struct{}
}

func (w *cleanupBlockingLog) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "clean up failed module runtime") {
		close(w.entered)
		<-w.release
	}
	return len(data), nil
}

func TestProjectCloseDrainsFailedRuntimeCleanupLogs(t *testing.T) {
	w := &cleanupBlockingLog{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(w.release) }) }
	defer release()
	log, _ := logging.New(w, logging.LevelInfo)
	m := New(&config.Config{}, log)
	rt := &moduleRuntime{status: StatusRunning, close: func() error { return errors.New("cleanup failed") }}
	m.modules[ModuleSVSub] = rt
	m.markRuntimeExit(ModuleSVSub, rt, errors.New("capture failed"))
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup log was not produced")
	}
	done := make(chan error, 1)
	go func() { done <- m.Close() }()
	select {
	case <-done:
		t.Fatal("project closed before cleanup diagnostics finished")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cleanup error was hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("project close did not finish")
	}
}
