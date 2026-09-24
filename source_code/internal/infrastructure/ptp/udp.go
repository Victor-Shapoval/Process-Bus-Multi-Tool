//go:build linux || darwin

package ptp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"pbmt/internal/application/ptpport"
	ptpdomain "pbmt/internal/domain/ptp"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const readDeadline = 100 * time.Millisecond

// UDPTransport is a PTP UDP multicast transport (ports 319/320).
type UDPTransport struct {
	iface         *net.Interface
	eventConn     *net.UDPConn
	generalConn   *net.UDPConn
	eventRawConn  syscall.RawConn
	mcastEvent    *net.UDPAddr
	mcastPDelay   *net.UDPAddr
	mcastGeneral  *net.UDPAddr
	timestampMode string
	timeConverter *hardwareClockConverter
	txMu          sync.Mutex
	txFailure     error // guarded by txMu; every timestamp mode is fail-closed

	eventCh   chan ptpport.Packet
	generalCh chan ptpport.Packet
	errors    chan error
	done      chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once
	closeOnce sync.Once
}

// NewUDP creates a UDP multicast transport.
func NewUDP(ifaceName, timestampMode string) (*UDPTransport, error) {
	return newUDP(ifaceName, timestampMode, timestampClockRealtime)
}

func newUDP(ifaceName, timestampMode string, clockMode timestampClockMode) (*UDPTransport, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("ptp transport: interface %q not found: %w", ifaceName, err)
	}

	group := net.ParseIP(ptpdomain.PrimaryMulticastIP)
	pdelay := net.ParseIP(ptpdomain.PDelayMulticastIP)

	eventConn, err := listenMulticast(iface, group, ptpdomain.UDPEventPort)
	if err != nil {
		return nil, fmt.Errorf("ptp transport: event socket: %w", err)
	}

	timeConverter, err := enableUDPKernelTimestamps(eventConn, iface.Name, timestampMode, clockMode)
	if err != nil {
		eventConn.Close()
		return nil, fmt.Errorf("ptp transport: enable event timestamps: %w", err)
	}
	var generalConn *net.UDPConn
	constructed := false
	defer func() {
		if constructed {
			return
		}
		_ = eventConn.Close()
		if generalConn != nil {
			_ = generalConn.Close()
		}
		if timeConverter != nil {
			timeConverter.Close()
		}
	}()

	// Join the P2P group on the same event socket.
	pc := ipv4.NewPacketConn(eventConn)
	if err := pc.JoinGroup(iface, &net.UDPAddr{IP: pdelay}); err != nil {
		return nil, fmt.Errorf("ptp transport: join pdelay group: %w", err)
	}

	generalConn, err = listenMulticast(iface, group, ptpdomain.UDPGeneralPort)
	if err != nil {
		return nil, fmt.Errorf("ptp transport: general socket: %w", err)
	}
	// General PTP messages do not participate in timestamp calculations. Leave
	// this socket without SO_TIMESTAMPING so its error queue cannot accumulate
	// unused TX records from Announce, FollowUp and delay responses.
	generalPC := ipv4.NewPacketConn(generalConn)
	if err := generalPC.JoinGroup(iface, &net.UDPAddr{IP: pdelay}); err != nil {
		return nil, fmt.Errorf("ptp transport: join general pdelay group: %w", err)
	}

	eventRawConn, err := eventConn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("ptp transport: event raw conn: %w", err)
	}
	transport := &UDPTransport{
		iface:         iface,
		eventConn:     eventConn,
		generalConn:   generalConn,
		eventRawConn:  eventRawConn,
		mcastEvent:    &net.UDPAddr{IP: group, Port: ptpdomain.UDPEventPort},
		mcastPDelay:   &net.UDPAddr{IP: pdelay, Port: ptpdomain.UDPEventPort},
		mcastGeneral:  &net.UDPAddr{IP: group, Port: ptpdomain.UDPGeneralPort},
		timestampMode: timestampMode,
		timeConverter: timeConverter,
		eventCh:       make(chan ptpport.Packet, chanBufSize),
		generalCh:     make(chan ptpport.Packet, chanBufSize),
		errors:        make(chan error, 4),
		done:          make(chan struct{}),
	}
	constructed = true
	return transport, nil
}

func listenMulticast(iface *net.Interface, group net.IP, port int) (*net.UDPConn, error) {
	listenConfig := net.ListenConfig{Control: func(_, _ string, rawConn syscall.RawConn) error {
		var sockErr error
		if err := rawConn.Control(func(fd uintptr) {
			if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
				sockErr = err
				return
			}
			if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
				sockErr = err
			}
		}); err != nil {
			return err
		}
		return sockErr
	}}
	packetConn, err := listenConfig.ListenPacket(context.Background(), "udp4", (&net.UDPAddr{IP: net.IPv4zero, Port: port}).String())
	if err != nil {
		return nil, fmt.Errorf("listen UDP port %d: %w", port, err)
	}
	conn, ok := packetConn.(*net.UDPConn)
	if !ok {
		packetConn.Close()
		return nil, fmt.Errorf("listen UDP port %d: unexpected connection type %T", port, packetConn)
	}
	pc := ipv4.NewPacketConn(conn)
	if err := pc.JoinGroup(iface, &net.UDPAddr{IP: group}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("join multicast %s on %s: %w", group, iface.Name, err)
	}
	if err := pc.SetMulticastInterface(iface); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set multicast interface %s: %w", iface.Name, err)
	}
	if err := pc.SetMulticastTTL(1); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set multicast TTL=1: %w", err)
	}
	if err := pc.SetMulticastLoopback(false); err != nil {
		conn.Close()
		return nil, fmt.Errorf("disable multicast loopback: %w", err)
	}
	return conn, nil
}

func (t *UDPTransport) Start() {
	t.startOnce.Do(func() {
		t.wg.Add(2)
		go func() { defer t.wg.Done(); t.readLoop(t.eventConn, t.eventCh, true) }()
		go func() { defer t.wg.Done(); t.readLoop(t.generalConn, t.generalCh, false) }()
	})
}

func (t *UDPTransport) readLoop(conn *net.UDPConn, ch chan<- ptpport.Packet, requireTimestamp bool) {
	buf := make([]byte, recvBufSize)
	oob := make([]byte, oobSize)
	for {
		select {
		case <-t.done:
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(readDeadline))
		n, oobn, _, addr, err := conn.ReadMsgUDP(buf, oob)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			t.reportError(fmt.Errorf("ptp transport: UDP receive: %w", err))
			return
		}
		var rxTime time.Time
		if requireTimestamp {
			rxTime, err = extractKernelTimestamp(oob[:oobn], t.timestampMode, t.timeConverter)
			if err != nil {
				t.reportError(fmt.Errorf("ptp transport: UDP receive timestamp: %w", err))
				return
			}
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		pkt := ptpport.Packet{Data: data, From: addr, Timestamp: rxTime}
		select {
		case ch <- pkt:
		default:
		}
	}
}

func (t *UDPTransport) SendEvent(data []byte) (time.Time, error) {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	if t.txFailure != nil {
		return time.Time{}, t.txFailure
	}
	dst := t.mcastEvent
	if isPDelayEvent(data) {
		dst = t.mcastPDelay
	}
	drainTXErrQueueConn(t.eventRawConn)
	err := t.writeEvent(data, dst)
	if err != nil {
		return time.Time{}, t.recordTXFailure(err)
	}
	txTime, err := readTXTimestampConn(t.eventRawConn, t.timestampMode, t.timeConverter)
	return txTime, t.recordTXFailure(err)
}

func (t *UDPTransport) writeEvent(data []byte, dst *net.UDPAddr) error {
	oob := txTimestampControlMessage(t.timestampMode)
	if len(oob) == 0 {
		_, err := t.eventConn.WriteToUDP(data, dst)
		return err
	}
	_, _, err := t.eventConn.WriteMsgUDP(data, oob, dst)
	return err
}

func (t *UDPTransport) SendGeneral(data []byte) error {
	dst := t.mcastGeneral
	if isPDelayGeneral(data) {
		dst = &net.UDPAddr{IP: t.mcastPDelay.IP, Port: ptpdomain.UDPGeneralPort}
	}
	_, err := t.generalConn.WriteToUDP(data, dst)
	return err
}

func (t *UDPTransport) SendEventTo(data []byte, dst net.Addr) (time.Time, error) {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	if t.txFailure != nil {
		return time.Time{}, t.txFailure
	}
	udpAddr, ok := dst.(*net.UDPAddr)
	if !ok {
		return time.Time{}, fmt.Errorf("ptp transport: SendEventTo requires *net.UDPAddr, got %T", dst)
	}
	drainTXErrQueueConn(t.eventRawConn)
	err := t.writeEvent(data, udpAddr)
	if err != nil {
		return time.Time{}, t.recordTXFailure(err)
	}
	txTime, err := readTXTimestampConn(t.eventRawConn, t.timestampMode, t.timeConverter)
	return txTime, t.recordTXFailure(err)
}

func (t *UDPTransport) SendGeneralTo(data []byte, dst net.Addr) error {
	udpAddr, err := udpGeneralDestination(dst)
	if err != nil {
		return fmt.Errorf("ptp transport: SendGeneralTo: %w", err)
	}
	_, err = t.generalConn.WriteToUDP(data, udpAddr)
	return err
}

func (t *UDPTransport) EventCh() <-chan ptpport.Packet   { return t.eventCh }
func (t *UDPTransport) GeneralCh() <-chan ptpport.Packet { return t.generalCh }
func (t *UDPTransport) Errors() <-chan error             { return t.errors }
func (t *UDPTransport) TimestampMode() string            { return t.timestampMode }

func (t *UDPTransport) Now() (time.Time, error) {
	if t.timeConverter != nil {
		return t.timeConverter.Now()
	}
	return time.Now().UTC(), nil
}

// recordTXFailure permanently stops event transmission after the
// first send/timestamp error. Otherwise a timestamp that arrives after a
// timeout could be mistaken for the next packet's timestamp.
// The caller holds txMu.
func (t *UDPTransport) recordTXFailure(err error) error {
	if err == nil {
		return err
	}
	if t.txFailure == nil {
		t.txFailure = fmt.Errorf("ptp transport: %s TX failed closed: %w", t.timestampMode, err)
	}
	return t.txFailure
}

func (t *UDPTransport) reportError(err error) {
	select {
	case t.errors <- err:
	default:
	}
}

func (t *UDPTransport) Close() {
	t.closeOnce.Do(func() {
		close(t.done)
		_ = t.eventConn.Close()
		_ = t.generalConn.Close()
		t.wg.Wait()
		if t.timeConverter != nil {
			t.timeConverter.Close()
		}
		close(t.errors)
	})
}
