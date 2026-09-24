package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pbmt/internal/application/goosepub"
	"pbmt/internal/application/goosesub"
	"pbmt/internal/config"
	"pbmt/internal/domain/goose"
)

type discardFrameSink struct{}

func (discardFrameSink) WriteFrame([]byte) error { return nil }

func TestConcurrentStartCreatesOneRuntime(t *testing.T) {
	m := New(&config.Config{}, nil)
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	m.startModule = func(ModuleID, *config.Config) (*moduleRuntime, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return &moduleRuntime{status: StatusRunning}, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.Start(ModuleGooseSub)
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("starter called concurrently: got %d calls before release", got)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("starter calls: want 1, got %d", got)
	}
}

func TestCloseStopsRunningModuleAndIsIdempotent(t *testing.T) {
	m := New(&config.Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var closeCalls atomic.Int32
	rt := &moduleRuntime{
		status: StatusRunning,
		cancel: cancel,
		close: func() error {
			closeCalls.Add(1)
			return nil
		},
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		<-ctx.Done()
	}()
	m.modules[ModuleGooseSub] = rt

	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if status := m.Status(ModuleGooseSub); status.State != StatusStopped {
		t.Fatalf("module status after Close: want %s, got %s", StatusStopped, status.State)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close calls after first Close: want 1, got %d", got)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close calls after second Close: want 1, got %d", got)
	}
}

func TestStartAfterCloseReturnsError(t *testing.T) {
	m := New(&config.Config{}, nil)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ModuleGooseSub); err == nil {
		t.Fatal("Start after Close unexpectedly succeeded")
	}
}

func TestSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	m := New(&config.Config{}, nil)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	events, cancel := m.Subscribe(1)
	cancel()
	if _, ok := <-events; ok {
		t.Fatal("Subscribe after Close returned an open channel")
	}
}

func TestUnexpectedRuntimeExitSetsError(t *testing.T) {
	m := New(&config.Config{}, nil)
	rt := &moduleRuntime{status: StatusRunning}
	m.modules[ModuleSVSub] = rt

	want := errors.New("capture failed")
	if got := m.markRuntimeExit(ModuleSVSub, rt, want); !errors.Is(got, want) {
		t.Fatalf("exit error: want %v, got %v", want, got)
	}
	status := m.Status(ModuleSVSub)
	if status.State != StatusError || status.Error != want.Error() {
		t.Fatalf("status: %+v", status)
	}
}

func TestStoppedRuntimeIgnoresExpectedExit(t *testing.T) {
	m := New(&config.Config{}, nil)
	rt := &moduleRuntime{status: StatusStopped}
	if err := m.markRuntimeExit(ModuleSVSub, rt, nil); err != nil {
		t.Fatalf("expected stop reported as error: %v", err)
	}
}

func TestStopWaitsForErroredRuntimeShutdown(t *testing.T) {
	m := New(&config.Config{}, nil)
	release := make(chan struct{})
	rt := &moduleRuntime{status: StatusRunning}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		<-release
	}()
	m.modules[ModuleSVSub] = rt

	if err := m.markRuntimeExit(ModuleSVSub, rt, errors.New("capture failed")); err == nil {
		t.Fatal("unexpected runtime exit returned nil")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop(ModuleSVSub) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before runtime goroutine exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestStartWaitsForFailedRuntimeCleanupBeforeReplacement(t *testing.T) {
	m := New(&config.Config{}, nil)
	release := make(chan struct{})
	old := &moduleRuntime{status: StatusError, err: "failed"}
	old.wg.Add(1)
	go func() {
		defer old.wg.Done()
		<-release
	}()
	m.modules[ModuleGooseSub] = old

	started := make(chan struct{}, 1)
	m.startModule = func(ModuleID, *config.Config) (*moduleRuntime, error) {
		started <- struct{}{}
		return &moduleRuntime{status: StatusRunning}, nil
	}
	result := make(chan error, 1)
	go func() { result <- m.Start(ModuleGooseSub) }()
	select {
	case <-started:
		t.Fatal("replacement runtime started before failed runtime cleanup")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	default:
		t.Fatal("replacement runtime was not started")
	}
}

func TestAutomaticPTPIdentitiesDifferByRole(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if len(iface.HardwareAddr) < 6 {
			continue
		}
		server, err := resolvePTPClockIdentity(iface.Name, false)
		if err != nil {
			t.Fatal(err)
		}
		client, err := resolvePTPClockIdentity(iface.Name, true)
		if err != nil {
			t.Fatal(err)
		}
		if server == client {
			t.Fatalf("automatic identities on %s are equal: %s", iface.Name, server)
		}
		return
	}
	t.Skip("no interface with a hardware address")
}

func TestApplyGoosePublisherStateIsAtomicAndDetectsDuplicates(t *testing.T) {
	initial := []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: false}}
	svc := goosepub.New(goosepub.PublisherConfig{InitialData: initial}, discardFrameSink{}, slog.Default(), nil)
	m := New(&config.Config{}, nil)
	m.modules[ModuleGoosePub] = &moduleRuntime{
		status:    StatusRunning,
		goosePubs: map[string]*goosepub.Service{"pub1": svc},
	}

	updated := []goose.DataValue{{Type: goose.DataTypeBoolean, Bool: true}}
	changed, err := m.ApplyGoosePublisherState("pub1", updated, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("changed state was reported as unchanged")
	}

	changed, err = m.ApplyGoosePublisherState("pub1", updated, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("duplicate state was reported as changed")
	}
}

func TestGooseEventSinkPublishesDecodedDataset(t *testing.T) {
	manager := New(&config.Config{}, nil)
	events, cancel := manager.Subscribe(1)
	defer cancel()

	pdu := &goose.PDU{
		AppID:            1,
		DatSet:           "IED1/LLN0$DataSet1",
		NumDatSetEntries: 2,
		AllData: []goose.DataValue{
			{Type: goose.DataTypeBoolean, Bool: true},
			{Type: goose.DataTypeInteger, Int: -7},
		},
	}
	receivedAt := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	gooseEventSink{manager: manager}.OnGooseEvent(goosesub.Event{
		Reason:       goosesub.ReasonFirstSeen,
		Subscription: "sub1",
		PDU:          pdu,
		ReceivedAt:   receivedAt,
	})

	event := <-events
	if event.Module != ModuleGooseSub || event.Subscription != "sub1" {
		t.Fatalf("unexpected runtime event: %+v", event)
	}
	if event.GoosePDU != pdu {
		t.Fatal("runtime event does not contain the decoded GOOSE PDU")
	}
	if len(event.GoosePDU.AllData) != 2 || !event.GoosePDU.AllData[0].Bool || event.GoosePDU.AllData[1].Int != -7 {
		t.Fatalf("decoded Dataset was not preserved: %+v", event.GoosePDU.AllData)
	}
}

func TestBuildGooseSubscriptionsMapsCompleteFilter(t *testing.T) {
	confRev := uint32(7)
	vlanID := uint16(100)
	vlanPri := uint8(4)
	subs, err := buildGooseSubscriptions([]config.GooseSubscriber{{
		Name:             "sub1",
		DstMAC:           "01:0c:cd:01:00:01",
		SrcMAC:           "00:11:22:33:44:55",
		AppID:            0x0001,
		GocbRef:          "IED1/LLN0$GO$Control",
		DatSet:           "IED1/LLN0$DataSet1",
		GoID:             "CTRL1",
		ConfRev:          &confRev,
		VLANID:           &vlanID,
		VLANPri:          &vlanPri,
		AcceptTest:       true,
		AcceptSimulation: true,
		AcceptNdsCom:     true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("subscriptions: want 1, got %d", len(subs))
	}
	sub := subs[0]
	if sub.DatSet != "IED1/LLN0$DataSet1" || sub.ConfRev == nil || *sub.ConfRev != confRev {
		t.Fatalf("SCL identity was not mapped: %+v", sub)
	}
	if sub.VLANID == nil || *sub.VLANID != vlanID || sub.VLANPriority == nil || *sub.VLANPriority != vlanPri {
		t.Fatalf("VLAN filter was not mapped: %+v", sub)
	}
	if !sub.AcceptTest || !sub.AcceptSimulation || !sub.AcceptNdsCom {
		t.Fatalf("diagnostic acceptance flags were not mapped: %+v", sub)
	}
}
