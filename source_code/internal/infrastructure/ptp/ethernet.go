//go:build linux

package ptp

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"pbmt/internal/application/ptpport"
	ptpdomain "pbmt/internal/domain/ptp"

	"golang.org/x/sys/unix"
)

const (
	etherHeaderSize = 14
	etherTypeVLAN   = 0x8100
	vlanTagSize     = 4
)

// EthernetTransport is a PTP L2 Ethernet transport (EtherType 0x88F7, AF_PACKET).
type EthernetTransport struct {
	iface         *net.Interface
	fd            int
	srcMAC        [6]byte
	timestampMode string
	timeConverter *hardwareClockConverter
	txTimestamps  bool
	txMu          sync.Mutex
	txFailure     error // guarded by txMu; every timestamp mode is fail-closed
	eventCh       chan ptpport.Packet
	generalCh     chan ptpport.Packet
	errors        chan error
	done          chan struct{}
	wg            sync.WaitGroup
	startOnce     sync.Once
	closeOnce     sync.Once
}

// NewEthernet creates a PTP L2 transport.
func NewEthernet(ifaceName, timestampMode string) (*EthernetTransport, error) {
	return newEthernet(ifaceName, timestampMode, timestampClockRealtime)
}

func newEthernet(ifaceName, timestampMode string, clockMode timestampClockMode) (*EthernetTransport, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("ptp transport: interface %q not found: %w", ifaceName, err)
	}
	if len(iface.HardwareAddr) < 6 {
		return nil, fmt.Errorf("ptp transport: interface %q has no MAC address", ifaceName)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("ptp transport: AF_PACKET socket: %w", err)
	}

	sa := unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ALL),
		Ifindex:  iface.Index,
	}
	if err := unix.Bind(fd, &sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("ptp transport: bind AF_PACKET: %w", err)
	}
	// Binding directly to EtherType 0x88F7 would exclude VLAN-tagged frames,
	// whose outer EtherType is 0x8100. Keep ETH_P_ALL for correct VLAN
	// delivery, but reject unrelated traffic in the kernel.
	if err := attachPTPSocketFilter(fd); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("ptp transport: attach PTP socket filter: %w", err)
	}

	// Promiscuous mode for the PDelay MAC 01:80:C2:00:00:0E.
	promiscReq := unix.PacketMreq{
		Ifindex: int32(iface.Index),
		Type:    unix.PACKET_MR_PROMISC,
	}
	if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &promiscReq); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("ptp transport: set promiscuous: %w", err)
	}

	// L2 multicast groups
	for _, mac := range [][6]byte{ptpdomain.PrimaryMulticastMAC, ptpdomain.PDelayMulticastMAC} {
		mreq := unix.PacketMreq{
			Ifindex: int32(iface.Index),
			Type:    unix.PACKET_MR_MULTICAST,
			Alen:    6,
		}
		copy(mreq.Address[:], mac[:])
		if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mreq); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("ptp transport: join L2 multicast %x: %w", mac, err)
		}
	}

	tv := unix.Timeval{Sec: 0, Usec: 100_000}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("ptp transport: SO_RCVTIMEO: %w", err)
	}

	timeConverter, tsErr := enableEthernetKernelTimestamps(fd, iface.Name, timestampMode, clockMode)
	if tsErr != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("ptp transport: enable %s timestamps: %w", timestampMode, tsErr)
	}

	var srcMAC [6]byte
	copy(srcMAC[:], iface.HardwareAddr[:6])

	return &EthernetTransport{
		iface:         iface,
		fd:            fd,
		srcMAC:        srcMAC,
		timestampMode: timestampMode,
		timeConverter: timeConverter,
		txTimestamps:  true,
		eventCh:       make(chan ptpport.Packet, chanBufSize),
		generalCh:     make(chan ptpport.Packet, chanBufSize),
		errors:        make(chan error, 4),
		done:          make(chan struct{}),
	}, nil
}

func attachPTPSocketFilter(fd int) error {
	filter := []unix.SockFilter{
		{Code: uint16(unix.BPF_LD | unix.BPF_H | unix.BPF_ABS), K: 12},
		{Code: uint16(unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K), Jt: 3, K: uint32(ptpdomain.EtherTypePTP)},
		{Code: uint16(unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K), Jf: 3, K: etherTypeVLAN},
		{Code: uint16(unix.BPF_LD | unix.BPF_H | unix.BPF_ABS), K: 16},
		{Code: uint16(unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K), Jf: 1, K: uint32(ptpdomain.EtherTypePTP)},
		{Code: uint16(unix.BPF_RET | unix.BPF_K), K: 0xFFFFFFFF},
		{Code: uint16(unix.BPF_RET | unix.BPF_K), K: 0},
	}
	program := &unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	return unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, program)
}

func (t *EthernetTransport) Start() {
	t.startOnce.Do(func() {
		t.wg.Add(1)
		go func() { defer t.wg.Done(); t.readLoop() }()
	})
}

func (t *EthernetTransport) readLoop() {
	buf := make([]byte, recvBufSize)
	oob := make([]byte, oobSize)
	for {
		select {
		case <-t.done:
			return
		default:
		}
		n, oobn, _, _, err := unix.Recvmsg(t.fd, buf, oob, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
				continue
			}
			t.reportError(fmt.Errorf("ptp transport: Ethernet receive: %w", err))
			return
		}
		if n < etherHeaderSize {
			continue
		}
		// check EtherType (with VLAN support)
		etherType := binary.BigEndian.Uint16(buf[12:14])
		if etherType == etherTypeVLAN {
			if n < etherHeaderSize+vlanTagSize {
				continue
			}
			etherType = binary.BigEndian.Uint16(buf[16:18])
		}
		if etherType != ptpdomain.EtherTypePTP {
			continue
		}
		var frameSource [6]byte
		copy(frameSource[:], buf[6:12])
		// AF_PACKET delivers locally transmitted frames to the socket too. They
		// do not carry an RX hardware timestamp and are irrelevant to the local
		// PTP endpoint, so discard them before strict timestamp validation.
		if frameSource == t.srcMAC {
			continue
		}

		hdrLen := etherHeaderSize
		if binary.BigEndian.Uint16(buf[12:14]) == etherTypeVLAN {
			hdrLen = etherHeaderSize + vlanTagSize
		}
		if n < hdrLen+1 {
			continue
		}

		payload := make([]byte, n-hdrLen)
		copy(payload, buf[hdrLen:n])
		eventMessage := payload[0]&0x0F <= ptpdomain.MsgPDelayResp
		var rxTime time.Time
		if eventMessage {
			rxTime, err = extractKernelTimestamp(oob[:oobn], t.timestampMode, t.timeConverter)
			if err != nil {
				t.reportError(fmt.Errorf("ptp transport: Ethernet receive timestamp: %w", err))
				return
			}
		}

		srcMAC := make(hwAddr, 6)
		copy(srcMAC, frameSource[:])

		pkt := ptpport.Packet{Data: payload, From: srcMAC, Timestamp: rxTime}

		var ch chan ptpport.Packet
		if eventMessage {
			ch = t.eventCh
		} else {
			ch = t.generalCh
		}
		select {
		case ch <- pkt:
		default:
		}
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

func (t *EthernetTransport) sendFrame(frame []byte) error {
	sa := &unix.SockaddrLinklayer{
		Protocol: htons(ptpdomain.EtherTypePTP),
		Ifindex:  t.iface.Index,
	}
	return unix.Sendto(t.fd, frame, 0, sa)
}

func (t *EthernetTransport) sendEventFrame(frame []byte) error {
	oob := txTimestampControlMessage(t.timestampMode)
	if len(oob) == 0 {
		return t.sendFrame(frame)
	}
	sa := &unix.SockaddrLinklayer{
		Protocol: htons(ptpdomain.EtherTypePTP),
		Ifindex:  t.iface.Index,
	}
	_, err := unix.SendmsgN(t.fd, frame, oob, sa, 0)
	return err
}

func (t *EthernetTransport) SendEvent(data []byte) (time.Time, error) {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	if t.txFailure != nil {
		return time.Time{}, t.txFailure
	}
	dst := ptpdomain.PrimaryMulticastMAC
	if isPDelayEvent(data) {
		dst = ptpdomain.PDelayMulticastMAC
	}
	if t.txTimestamps {
		drainTXErrQueue(t.fd)
	}
	err := t.sendEventFrame(t.buildFrame(dst, data))
	if err != nil {
		return time.Time{}, t.recordTXFailure(err)
	}
	if t.txTimestamps {
		txTime, err := readTXTimestamp(t.fd, t.timestampMode, t.timeConverter)
		return txTime, t.recordTXFailure(err)
	}
	return time.Time{}, t.recordTXFailure(fmt.Errorf("ptp transport: %s TX timestamps are disabled", t.timestampMode))
}

func (t *EthernetTransport) SendGeneral(data []byte) error {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	dst := ptpdomain.PrimaryMulticastMAC
	if isPDelayGeneral(data) {
		dst = ptpdomain.PDelayMulticastMAC
	}
	return t.sendFrame(t.buildFrame(dst, data))
}

func (t *EthernetTransport) SendEventTo(data []byte, dst net.Addr) (time.Time, error) {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	if t.txFailure != nil {
		return time.Time{}, t.txFailure
	}
	mac, err := hwAddrFrom(dst)
	if err != nil {
		return time.Time{}, fmt.Errorf("ptp transport: SendEventTo: %w", err)
	}
	if t.txTimestamps {
		drainTXErrQueue(t.fd)
	}
	err = t.sendEventFrame(t.buildFrame(mac, data))
	if err != nil {
		return time.Time{}, t.recordTXFailure(err)
	}
	if t.txTimestamps {
		txTime, err := readTXTimestamp(t.fd, t.timestampMode, t.timeConverter)
		return txTime, t.recordTXFailure(err)
	}
	return time.Time{}, t.recordTXFailure(fmt.Errorf("ptp transport: %s TX timestamps are disabled", t.timestampMode))
}

func (t *EthernetTransport) SendGeneralTo(data []byte, dst net.Addr) error {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	mac, err := hwAddrFrom(dst)
	if err != nil {
		return fmt.Errorf("ptp transport: SendGeneralTo: %w", err)
	}
	return t.sendFrame(t.buildFrame(mac, data))
}

func (t *EthernetTransport) EventCh() <-chan ptpport.Packet   { return t.eventCh }
func (t *EthernetTransport) GeneralCh() <-chan ptpport.Packet { return t.generalCh }
func (t *EthernetTransport) Errors() <-chan error             { return t.errors }
func (t *EthernetTransport) TimestampMode() string            { return t.timestampMode }

func (t *EthernetTransport) Now() (time.Time, error) {
	if t.timeConverter != nil {
		return t.timeConverter.Now()
	}
	return time.Now().UTC(), nil
}

// recordTXFailure permanently stops event transmission after the
// first send/timestamp error. Otherwise a timestamp that arrives after a
// timeout could be mistaken for the next packet's timestamp.
// The caller holds txMu.
func (t *EthernetTransport) recordTXFailure(err error) error {
	if err == nil {
		return err
	}
	if t.txFailure == nil {
		t.txFailure = fmt.Errorf("ptp transport: %s TX failed closed: %w", t.timestampMode, err)
	}
	return t.txFailure
}

func (t *EthernetTransport) reportError(err error) {
	select {
	case t.errors <- err:
	default:
	}
}

func (t *EthernetTransport) Close() {
	t.closeOnce.Do(func() {
		close(t.done)
		_ = unix.Close(t.fd)
		t.wg.Wait()
		if t.timeConverter != nil {
			t.timeConverter.Close()
		}
		close(t.errors)
	})
}

// hwAddr wraps net.HardwareAddr and implements net.Addr.
type hwAddr net.HardwareAddr

func (h hwAddr) Network() string { return "ethernet" }
func (h hwAddr) String() string  { return net.HardwareAddr(h).String() }

func hwAddrFrom(addr net.Addr) ([6]byte, error) {
	switch v := addr.(type) {
	case hwAddr:
		if len(v) < 6 {
			return [6]byte{}, fmt.Errorf("expected hwAddr length>=6, got %d", len(v))
		}
		var mac [6]byte
		copy(mac[:], v[:6])
		return mac, nil
	default:
		return [6]byte{}, fmt.Errorf("expected hwAddr, got %T", addr)
	}
}

func htons(v uint16) uint16 { return (v << 8) | (v >> 8) }
