//go:build darwin

package ptp

import (
	"errors"
	"os"
	"testing"
	"time"

	"pbmt/internal/application/ptpport"
	ptpdomain "pbmt/internal/domain/ptp"
)

type fakeEthernetFrameInjector struct {
	write func([]byte) error
}

func (f *fakeEthernetFrameInjector) WriteFrame(frame []byte) error {
	if f.write == nil {
		return nil
	}
	return f.write(frame)
}

func (*fakeEthernetFrameInjector) Close() error { return nil }

func newDarwinTestTransport(injector ethernetFrameInjector) *EthernetTransport {
	return &EthernetTransport{
		injector:      injector,
		srcMAC:        [6]byte{0x68, 0x2F, 0x67, 0x92, 0x78, 0x3F},
		timestampMode: "software",
		txTimeout:     10 * time.Millisecond,
		eventCh:       make(chan ptpport.Packet, 1),
		generalCh:     make(chan ptpport.Packet, 1),
		errors:        make(chan error, 2),
		done:          make(chan struct{}),
	}
}

func testSyncPayload() []byte {
	return ptpdomain.EncodeSync(ptpdomain.Header{
		MessageType: ptpdomain.MsgSync,
		VersionPTP:  ptpdomain.PTPVersion2,
		SequenceID:  42,
	}, ptpdomain.SyncBody{})
}

func TestDarwinEthernetEventTXUsesCapturedBPFTimestamp(t *testing.T) {
	want := time.Date(2026, 8, 7, 12, 0, 0, 123_456_000, time.UTC)
	injector := &fakeEthernetFrameInjector{}
	transport := newDarwinTestTransport(injector)
	injector.write = func(frame []byte) error {
		captured := append(append([]byte(nil), frame...), 0, 0) // Ethernet padding is allowed.
		go transport.handleFrame(captured, want)
		return nil
	}

	got, err := transport.SendEvent(testSyncPayload())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("TX timestamp: want %v, got %v", want, got)
	}
	select {
	case packet := <-transport.eventCh:
		t.Fatalf("locally transmitted frame leaked into EventCh: %+v", packet)
	default:
	}
}

func TestDarwinEthernetUnrelatedCaptureDoesNotCompleteTX(t *testing.T) {
	want := time.Date(2026, 8, 7, 12, 0, 0, 654_321_000, time.UTC)
	injector := &fakeEthernetFrameInjector{}
	transport := newDarwinTestTransport(injector)
	injector.write = func(frame []byte) error {
		unrelated := append([]byte(nil), frame...)
		unrelated[len(unrelated)-1] ^= 0xFF
		go func() {
			transport.handleFrame(unrelated, want.Add(-time.Millisecond))
			transport.handleFrame(frame, want)
		}()
		return nil
	}

	got, err := transport.SendEvent(testSyncPayload())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("TX timestamp: want %v, got %v", want, got)
	}
}

func TestDarwinEthernetMissingTXTimestampFailsClosed(t *testing.T) {
	writes := 0
	injector := &fakeEthernetFrameInjector{write: func([]byte) error {
		writes++
		return nil
	}}
	transport := newDarwinTestTransport(injector)
	transport.txTimeout = time.Millisecond

	got, firstErr := transport.SendEvent(testSyncPayload())
	if !got.IsZero() {
		t.Fatalf("missing timestamp returned %v", got)
	}
	if !errors.Is(firstErr, ErrSoftwareTimestampMissing) {
		t.Fatalf("expected missing software TX timestamp, got %v", firstErr)
	}
	if _, secondErr := transport.SendEvent(testSyncPayload()); secondErr != firstErr {
		t.Fatalf("TX failure was not latched: first=%v second=%v", firstErr, secondErr)
	}
	if writes != 1 {
		t.Fatalf("failed transport retried injection: writes=%d", writes)
	}
}

func TestDarwinEthernetZeroCapturedTXTimestampIsRejected(t *testing.T) {
	injector := &fakeEthernetFrameInjector{}
	transport := newDarwinTestTransport(injector)
	injector.write = func(frame []byte) error {
		go transport.handleFrame(frame, time.Time{})
		return nil
	}

	got, err := transport.SendEvent(testSyncPayload())
	if !got.IsZero() {
		t.Fatalf("zero capture timestamp returned %v", got)
	}
	if !errors.Is(err, ErrSoftwareTimestampMissing) {
		t.Fatalf("expected missing software TX timestamp, got %v", err)
	}
}

func TestDarwinEthernetZeroRXTimestampIsRejected(t *testing.T) {
	transport := newDarwinTestTransport(&fakeEthernetFrameInjector{})
	frame := transport.buildFrame(ptpdomain.PrimaryMulticastMAC, testSyncPayload())
	copy(frame[6:12], []byte{0x02, 0, 0, 0, 0, 1})

	transport.handleFrame(frame, time.Time{})
	select {
	case packet := <-transport.eventCh:
		t.Fatalf("packet without timestamp reached EventCh: %+v", packet)
	default:
	}
	select {
	case err := <-transport.errors:
		if !errors.Is(err, ErrSoftwareTimestampMissing) {
			t.Fatalf("expected missing software RX timestamp, got %v", err)
		}
	default:
		t.Fatal("missing RX timestamp was not reported")
	}
}

func TestDarwinEthernetLiveSoftwareTXTimestamp(t *testing.T) {
	iface := os.Getenv("PBMT_PTP_TEST_INTERFACE")
	if iface == "" {
		t.Skip("set PBMT_PTP_TEST_INTERFACE to run the live BPF timestamp test")
	}
	transport, err := NewEthernet(iface, "software")
	if err != nil {
		t.Fatal(err)
	}
	transport.Start()
	defer transport.Close()

	payload := ptpdomain.EncodeSync(ptpdomain.Header{
		MessageType:  ptpdomain.MsgSync,
		VersionPTP:   ptpdomain.PTPVersion2,
		DomainNumber: 255, // non-default domain avoids joining the working PTP domain
		SequenceID:   0xBEEF,
	}, ptpdomain.SyncBody{})
	timestamp, err := transport.SendEvent(payload)
	if err != nil {
		t.Fatal(err)
	}
	if timestamp.IsZero() {
		t.Fatal("BPF returned a zero TX timestamp")
	}
	t.Logf("captured BPF TX timestamp on %s: %s", iface, timestamp.Format(time.RFC3339Nano))
}
