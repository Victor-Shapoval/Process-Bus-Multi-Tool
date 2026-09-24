package ptpclient

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"pbmt/internal/application/ptpport"
	"pbmt/internal/application/ptpserver"
	appclock "pbmt/internal/domain/clock"
	"pbmt/internal/domain/ptp"
)

type localPTPAddr string

func (a localPTPAddr) Network() string { return "local-ptp" }
func (a localPTPAddr) String() string  { return string(a) }

type localPTPBus struct {
	mu        sync.Mutex
	endpoints []*localPTPEndpoint
}

func (b *localPTPBus) endpoint(name string) *localPTPEndpoint {
	e := &localPTPEndpoint{
		bus:       b,
		addr:      localPTPAddr(name),
		eventCh:   make(chan ptpport.Packet, 128),
		generalCh: make(chan ptpport.Packet, 128),
		errors:    make(chan error),
	}
	b.mu.Lock()
	b.endpoints = append(b.endpoints, e)
	b.mu.Unlock()
	return e
}

func (b *localPTPBus) broadcast(from net.Addr, data []byte, event bool) {
	b.mu.Lock()
	endpoints := append([]*localPTPEndpoint(nil), b.endpoints...)
	b.mu.Unlock()
	packet := ptpport.Packet{Data: append([]byte(nil), data...), From: from, Timestamp: time.Now()}
	for _, endpoint := range endpoints {
		channel := endpoint.generalCh
		if event {
			channel = endpoint.eventCh
		}
		select {
		case channel <- packet:
		default:
		}
	}
}

type localPTPEndpoint struct {
	bus       *localPTPBus
	addr      net.Addr
	eventCh   chan ptpport.Packet
	generalCh chan ptpport.Packet
	errors    chan error
}

func (e *localPTPEndpoint) Start()                                   {}
func (e *localPTPEndpoint) Close()                                   {}
func (e *localPTPEndpoint) EventCh() <-chan ptpport.Packet           { return e.eventCh }
func (e *localPTPEndpoint) GeneralCh() <-chan ptpport.Packet         { return e.generalCh }
func (e *localPTPEndpoint) Errors() <-chan error                     { return e.errors }
func (e *localPTPEndpoint) TimestampMode() string                    { return "hardware" }
func (e *localPTPEndpoint) Now() (time.Time, error)                  { return time.Now().UTC(), nil }
func (e *localPTPEndpoint) SendEvent(data []byte) (time.Time, error) { return e.sendEvent(data) }
func (e *localPTPEndpoint) SendGeneral(data []byte) error {
	e.bus.broadcast(e.addr, data, false)
	return nil
}
func (e *localPTPEndpoint) SendEventTo(data []byte, _ net.Addr) (time.Time, error) {
	return e.sendEvent(data)
}
func (e *localPTPEndpoint) SendGeneralTo(data []byte, _ net.Addr) error {
	return e.SendGeneral(data)
}
func (e *localPTPEndpoint) sendEvent(data []byte) (time.Time, error) {
	timestamp := time.Now()
	e.bus.broadcast(e.addr, data, true)
	return timestamp, nil
}

func TestLocalServerSynchronizesClientWithDistinctAutomaticIdentities(t *testing.T) {
	profile := ptp.DefaultProfile
	profile.LogSyncInterval = -2
	profile.LogAnnounceInterval = -1
	profile.LogDelayReqInterval = -2
	profile.AnnounceReceiptTimeout = 3

	mac := [6]byte{0x68, 0x2F, 0x67, 0x92, 0x78, 0x3F}
	serverID := ptp.PortIdentity{ClockIdentity: ptp.ClockIdentityFromMAC(mac), PortNumber: 1}
	clientID := ptp.PortIdentity{ClockIdentity: ptp.ClientClockIdentityFromMAC(mac), PortNumber: 1}
	if serverID == clientID {
		t.Fatal("local Server and Client identities must differ")
	}

	bus := &localPTPBus{}
	serverTransport := bus.endpoint("server")
	clientTransport := bus.endpoint("client")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	applicationClock := appclock.NewApplicationClock(500_000)
	applicationClock.EnablePTP()
	server := ptpserver.New(serverTransport, serverID, ptpserver.Config{
		Profile:    profile,
		Transport:  "ethernet",
		UTCOffset:  37,
		TimeSource: ptp.TimeSourceGPS,
	}, logger)
	client := New(clientTransport, applicationClock, clientID, Config{
		Profile:       profile,
		Transport:     "ethernet",
		TimestampMode: "hardware",
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	clientDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { clientDone <- client.Run(ctx) }()
	go func() { serverDone <- server.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for client.State() != StateSlave && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if client.State() != StateSlave {
		finalState := client.State()
		cancel()
		<-clientDone
		<-serverDone
		t.Fatalf("client did not synchronize, final state %s", finalState)
	}
	snapshot := client.Snapshot()
	if snapshot.GrandmasterIdentity != serverID.ClockIdentity {
		t.Fatalf("selected Grandmaster: want %s, got %s", serverID.ClockIdentity, snapshot.GrandmasterIdentity)
	}
	if !snapshot.ClockStatus.Synchronized {
		t.Fatal("application clock is not marked synchronized")
	}

	cancel()
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestLocalServerSynchronizesTwoIndependentClients(t *testing.T) {
	profile := ptp.DefaultProfile
	profile.LogSyncInterval = -2
	profile.LogAnnounceInterval = -1
	profile.LogDelayReqInterval = -2
	profile.AnnounceReceiptTimeout = 3

	serverID := ptp.PortIdentity{
		ClockIdentity: ptp.ClockIdentityFromMAC([6]byte{0x68, 0x2F, 0x67, 0x92, 0x78, 0x3F}),
		PortNumber:    1,
	}
	clientIDs := []ptp.PortIdentity{
		{ClockIdentity: ptp.ClientClockIdentityFromMAC([6]byte{0x02, 0, 0, 0, 0, 1}), PortNumber: 1},
		{ClockIdentity: ptp.ClientClockIdentityFromMAC([6]byte{0x02, 0, 0, 0, 0, 2}), PortNumber: 1},
	}

	bus := &localPTPBus{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := ptpserver.New(bus.endpoint("server"), serverID, ptpserver.Config{
		Profile:    profile,
		Transport:  "ethernet",
		UTCOffset:  37,
		TimeSource: ptp.TimeSourceGPS,
	}, logger)

	clientNames := []string{"client-1", "client-2"}
	clients := make([]*Service, 0, len(clientIDs))
	for i, clientID := range clientIDs {
		applicationClock := appclock.NewApplicationClock(500_000)
		applicationClock.EnablePTP()
		clients = append(clients, New(bus.endpoint(clientNames[i]), applicationClock, clientID, Config{
			Profile:       profile,
			Transport:     "ethernet",
			TimestampMode: "hardware",
		}, logger))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, len(clients)+1)
	go func() { done <- server.Run(ctx) }()
	for _, client := range clients {
		client := client
		go func() { done <- client.Run(ctx) }()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allSynchronized := true
		for _, client := range clients {
			if client.State() != StateSlave || !client.Snapshot().ClockStatus.Synchronized {
				allSynchronized = false
				break
			}
		}
		if allSynchronized {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, client := range clients {
		snapshot := client.Snapshot()
		state := client.State()
		if state != StateSlave || !snapshot.ClockStatus.Synchronized {
			allSnapshots := make([]Snapshot, len(clients))
			for j, candidate := range clients {
				allSnapshots[j] = candidate.Snapshot()
			}
			cancel()
			for range len(clients) + 1 {
				<-done
			}
			t.Fatalf("client %d did not synchronize: state=%s clock=%+v snapshots=%+v", i+1, state, snapshot.ClockStatus, allSnapshots)
		}
		if snapshot.GrandmasterIdentity != serverID.ClockIdentity {
			cancel()
			for range len(clients) + 1 {
				<-done
			}
			t.Fatalf("client %d selected Grandmaster %s, want %s", i+1, snapshot.GrandmasterIdentity, serverID.ClockIdentity)
		}
	}

	cancel()
	for range len(clients) + 1 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
