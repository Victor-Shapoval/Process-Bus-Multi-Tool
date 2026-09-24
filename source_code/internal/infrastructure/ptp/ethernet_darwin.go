//go:build darwin

package ptp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"pbmt/internal/application/ptpport"
	ptpdomain "pbmt/internal/domain/ptp"
	"pbmt/internal/infrastructure/capture"
	"pbmt/internal/infrastructure/inject"
)

const (
	etherHeaderSize                 = 14
	etherTypeVLAN                   = 0x8100
	vlanTagSize                     = 4
	defaultDarwinTXTimestampTimeout = 100 * time.Millisecond
)

type ethernetFrameInjector interface {
	WriteFrame(frame []byte) error
	Close() error
}

type darwinTXTimestampResult struct {
	timestamp time.Time
	err       error
}

type darwinPendingTXTimestamp struct {
	frame  []byte
	result chan darwinTXTimestampResult
}

// EthernetTransport implements PTP over IEEE 802.3 on macOS through libpcap/BPF.
type EthernetTransport struct {
	iface         *net.Interface
	source        *capture.PcapSource
	injector      ethernetFrameInjector
	srcMAC        [6]byte
	timestampMode string
	txMu          sync.Mutex
	txFailure     error // guarded by txMu; prevents reuse of a late BPF timestamp
	pendingMu     sync.Mutex
	pendingTX     *darwinPendingTXTimestamp
	txTimeout     time.Duration
	eventCh       chan ptpport.Packet
	generalCh     chan ptpport.Packet
	errors        chan error
	done          chan struct{}
	wg            sync.WaitGroup
	startOnce     sync.Once
	closeOnce     sync.Once
}

func NewEthernet(ifaceName, timestampMode string) (*EthernetTransport, error) {
	if timestampMode == "hardware" {
		return nil, fmt.Errorf("%w on macOS interface %s", ErrHardwareTimestampingUnavailable, ifaceName)
	}
	if timestampMode != "software" {
		return nil, fmt.Errorf("ptp transport: unsupported timestamp mode %q", timestampMode)
	}
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("ptp transport: interface %q not found: %w", ifaceName, err)
	}
	if len(iface.HardwareAddr) < 6 {
		return nil, fmt.Errorf("ptp transport: interface %q has no MAC address", ifaceName)
	}
	source, err := capture.Open(capture.Config{
		Interface:   ifaceName,
		BPFFilter:   "ether proto 0x88f7 or (vlan and ether proto 0x88f7)",
		Promiscuous: true,
		Immediate:   true,
		Timeout:     100 * time.Millisecond,
		ChanBuffer:  chanBufSize,
	})
	if err != nil {
		return nil, fmt.Errorf("ptp transport: Ethernet capture: %w", err)
	}
	injector, err := inject.Open(inject.Config{Interface: ifaceName})
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("ptp transport: Ethernet injection: %w", err)
	}
	var srcMAC [6]byte
	copy(srcMAC[:], iface.HardwareAddr[:6])
	return &EthernetTransport{
		iface:         iface,
		source:        source,
		injector:      injector,
		srcMAC:        srcMAC,
		timestampMode: "software",
		eventCh:       make(chan ptpport.Packet, chanBufSize),
		generalCh:     make(chan ptpport.Packet, chanBufSize),
		errors:        make(chan error, 4),
		done:          make(chan struct{}),
		txTimeout:     defaultDarwinTXTimestampTimeout,
	}, nil
}

func (t *EthernetTransport) Start() {
	t.startOnce.Do(func() {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.readLoop()
		}()
	})
}

func (t *EthernetTransport) readLoop() {
	frames := t.source.Frames()
	errs := t.source.Errors()
	for frames != nil || errs != nil {
		select {
		case <-t.done:
			return
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			t.reportError(fmt.Errorf("ptp transport: Ethernet receive: %w", err))
			return
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			t.handleFrame(frame.Data, frame.Timestamp)
		}
	}
	select {
	case <-t.done:
	default:
		t.reportError(fmt.Errorf("ptp transport: Ethernet capture stopped"))
	}
}

func (t *EthernetTransport) handleFrame(frame []byte, timestamp time.Time) {
	if len(frame) < etherHeaderSize {
		return
	}
	headerLength := etherHeaderSize
	etherType := binary.BigEndian.Uint16(frame[12:14])
	if etherType == etherTypeVLAN {
		if len(frame) < etherHeaderSize+vlanTagSize {
			return
		}
		headerLength += vlanTagSize
		etherType = binary.BigEndian.Uint16(frame[16:18])
	}
	if etherType != ptpdomain.EtherTypePTP || len(frame) <= headerLength {
		return
	}
	if bytes.Equal(frame[6:12], t.srcMAC[:]) {
		t.completeTXTimestamp(frame, timestamp)
		return
	}
	if timestamp.IsZero() {
		t.reportError(fmt.Errorf("ptp transport: Ethernet receive timestamp: %w", timestampMissingError("software", "RX")))
		return
	}
	payload := append([]byte(nil), frame[headerLength:]...)
	from := make(hwAddr, 6)
	copy(from, frame[6:12])
	pkt := ptpport.Packet{Data: payload, From: from, Timestamp: timestamp}
	ch := t.generalCh
	if payload[0]&0x0F <= ptpdomain.MsgPDelayResp {
		ch = t.eventCh
	}
	select {
	case ch <- pkt:
	default:
	}
}

func (t *EthernetTransport) buildFrame(dst [6]byte, payload []byte) []byte {
	frame := make([]byte, etherHeaderSize+len(payload))
	copy(frame[0:6], dst[:])
	copy(frame[6:12], t.srcMAC[:])
	binary.BigEndian.PutUint16(frame[12:14], ptpdomain.EtherTypePTP)
	copy(frame[14:], payload)
	return frame
}

func (t *EthernetTransport) sendEventFrame(dst [6]byte, data []byte) (time.Time, error) {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	select {
	case <-t.done:
		return time.Time{}, fmt.Errorf("ptp transport: Ethernet transport is closed")
	default:
	}
	if t.txFailure != nil {
		return time.Time{}, t.txFailure
	}
	frame := t.buildFrame(dst, data)
	pending := &darwinPendingTXTimestamp{
		frame:  append([]byte(nil), frame...),
		result: make(chan darwinTXTimestampResult, 1),
	}
	if err := t.registerTXTimestamp(pending); err != nil {
		return time.Time{}, t.recordTXFailure(err)
	}
	if err := t.injector.WriteFrame(frame); err != nil {
		t.clearTXTimestamp(pending)
		return time.Time{}, t.recordTXFailure(fmt.Errorf("ptp transport: Ethernet event send: %w", err))
	}

	timeout := t.txTimeout
	if timeout <= 0 {
		timeout = defaultDarwinTXTimestampTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-pending.result:
		return result.timestamp, t.recordTXFailure(result.err)
	case <-timer.C:
		if t.clearTXTimestamp(pending) {
			err := fmt.Errorf("ptp transport: %w after %s", timestampMissingError("software", "TX"), timeout)
			return time.Time{}, t.recordTXFailure(err)
		}
		result := <-pending.result
		return result.timestamp, t.recordTXFailure(result.err)
	case <-t.done:
		if t.clearTXTimestamp(pending) {
			return time.Time{}, fmt.Errorf("ptp transport: Ethernet transport closed while waiting for TX timestamp")
		}
		result := <-pending.result
		return result.timestamp, result.err
	}
}

// recordTXFailure latches the first event TX error. Without packet IDs a late
// BPF copy after a timeout must never be allowed to match a later retry.
// The caller holds txMu.
func (t *EthernetTransport) recordTXFailure(err error) error {
	if err == nil {
		return nil
	}
	if t.txFailure == nil {
		t.txFailure = fmt.Errorf("ptp transport: software TX failed closed: %w", err)
	}
	return t.txFailure
}

func (t *EthernetTransport) sendGeneralFrame(dst [6]byte, data []byte) error {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	return t.injector.WriteFrame(t.buildFrame(dst, data))
}

func (t *EthernetTransport) registerTXTimestamp(pending *darwinPendingTXTimestamp) error {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	if t.pendingTX != nil {
		return fmt.Errorf("ptp transport: another Ethernet TX timestamp is pending")
	}
	t.pendingTX = pending
	return nil
}

// clearTXTimestamp returns true when this call removed the pending request.
// False means the capture loop already owns it and will publish the result.
func (t *EthernetTransport) clearTXTimestamp(pending *darwinPendingTXTimestamp) bool {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	if t.pendingTX != pending {
		return false
	}
	t.pendingTX = nil
	return true
}

func (t *EthernetTransport) completeTXTimestamp(frame []byte, timestamp time.Time) bool {
	t.pendingMu.Lock()
	pending := t.pendingTX
	if pending == nil || len(frame) < len(pending.frame) || !bytes.Equal(frame[:len(pending.frame)], pending.frame) {
		t.pendingMu.Unlock()
		return false
	}
	t.pendingTX = nil
	t.pendingMu.Unlock()

	result := darwinTXTimestampResult{timestamp: timestamp}
	if timestamp.IsZero() {
		result.err = fmt.Errorf("ptp transport: Ethernet transmit timestamp: %w", timestampMissingError("software", "TX"))
	}
	pending.result <- result
	return true
}

func (t *EthernetTransport) SendEvent(data []byte) (time.Time, error) {
	dst := ptpdomain.PrimaryMulticastMAC
	if isPDelayEvent(data) {
		dst = ptpdomain.PDelayMulticastMAC
	}
	return t.sendEventFrame(dst, data)
}

func (t *EthernetTransport) SendGeneral(data []byte) error {
	dst := ptpdomain.PrimaryMulticastMAC
	if isPDelayGeneral(data) {
		dst = ptpdomain.PDelayMulticastMAC
	}
	return t.sendGeneralFrame(dst, data)
}

func (t *EthernetTransport) SendEventTo(data []byte, dst net.Addr) (time.Time, error) {
	mac, err := hwAddrFrom(dst)
	if err != nil {
		return time.Time{}, fmt.Errorf("ptp transport: SendEventTo: %w", err)
	}
	return t.sendEventFrame(mac, data)
}

func (t *EthernetTransport) SendGeneralTo(data []byte, dst net.Addr) error {
	mac, err := hwAddrFrom(dst)
	if err != nil {
		return fmt.Errorf("ptp transport: SendGeneralTo: %w", err)
	}
	return t.sendGeneralFrame(mac, data)
}

func (t *EthernetTransport) EventCh() <-chan ptpport.Packet   { return t.eventCh }
func (t *EthernetTransport) GeneralCh() <-chan ptpport.Packet { return t.generalCh }
func (t *EthernetTransport) Errors() <-chan error             { return t.errors }
func (t *EthernetTransport) TimestampMode() string            { return t.timestampMode }

func (t *EthernetTransport) reportError(err error) {
	select {
	case t.errors <- err:
	default:
	}
}

func (t *EthernetTransport) Close() {
	t.closeOnce.Do(func() {
		close(t.done)
		_ = t.source.Close()
		_ = t.injector.Close()
		t.wg.Wait()
		close(t.errors)
	})
}

type hwAddr net.HardwareAddr

func (h hwAddr) Network() string { return "ethernet" }
func (h hwAddr) String() string  { return net.HardwareAddr(h).String() }

func hwAddrFrom(addr net.Addr) ([6]byte, error) {
	value, ok := addr.(hwAddr)
	if !ok || len(value) < 6 {
		return [6]byte{}, fmt.Errorf("expected Ethernet hardware address, got %T", addr)
	}
	var mac [6]byte
	copy(mac[:], value[:6])
	return mac, nil
}
