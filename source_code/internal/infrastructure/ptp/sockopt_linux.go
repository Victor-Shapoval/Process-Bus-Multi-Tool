//go:build linux

package ptp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	oobSize            = 128
	txTimestampTimeout = 100 * time.Millisecond

	soTimestampNS  int32 = 35
	soTimestamping int32 = 37

	sofTimestampingRxSW            = unix.SOF_TIMESTAMPING_RX_SOFTWARE | unix.SOF_TIMESTAMPING_SOFTWARE
	sofTimestampingRxHW            = unix.SOF_TIMESTAMPING_RX_HARDWARE | unix.SOF_TIMESTAMPING_RAW_HARDWARE
	sofTimestampingReportingOption = unix.SOF_TIMESTAMPING_OPT_TSONLY
)

func enableUDPKernelTimestamps(conn *net.UDPConn, ifaceName, mode string, clockMode timestampClockMode) (*hardwareClockConverter, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var setErr error
	var converter *hardwareClockConverter
	if err := rawConn.Control(func(fd uintptr) {
		switch mode {
		case "hardware":
			converter, setErr = configureHardwareTimestamping(int(fd), ifaceName, unix.HWTSTAMP_FILTER_PTP_V2_L4_EVENT, clockMode)
			if setErr != nil {
				return
			}
			flags := sofTimestampingRxHW | sofTimestampingReportingOption
			setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TIMESTAMPING, flags)
		case "software":
			flags := sofTimestampingRxSW | sofTimestampingReportingOption
			setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TIMESTAMPING, flags)
		default:
			setErr = fmt.Errorf("unsupported timestamp mode %q", mode)
		}
	}); err != nil {
		return nil, err
	}
	if setErr != nil && converter != nil {
		converter.Close()
		converter = nil
	}
	return converter, setErr
}

func enableEthernetKernelTimestamps(fd int, ifaceName, mode string, clockMode timestampClockMode) (*hardwareClockConverter, error) {
	switch mode {
	case "hardware":
		converter, err := configureHardwareTimestamping(fd, ifaceName, unix.HWTSTAMP_FILTER_PTP_V2_L2_EVENT, clockMode)
		if err != nil {
			return nil, err
		}
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING,
			sofTimestampingRxHW|sofTimestampingReportingOption); err != nil {
			converter.Close()
			return nil, err
		}
		return converter, nil
	case "software":
		return nil, unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING,
			sofTimestampingRxSW|sofTimestampingReportingOption)
	default:
		return nil, fmt.Errorf("unsupported timestamp mode %q", mode)
	}
}

type hardwareClockConverter struct {
	mu          sync.Mutex
	access      *hardwareClockAccess
	ifaceName   string
	closeOnce   sync.Once
	file        *os.File
	clockMode   timestampClockMode
	autonomous  *autonomousPHCClock
	readCross   func() (phcCrossTimestamp, error)
	readOffset  func() (time.Duration, error)
	readMonoraw func() (time.Time, error)
}

// A PHC can be shared by several transports. Serialize complete hardware
// cross-timestamp transactions, not just operations on each Go converter.
// Keep locks for the process lifetime so reopening a PHC cannot create two locks.
var hardwareClockAccesses sync.Map

type timestampLease struct {
	config unix.HwTstampConfig
	users  int
}

type hardwareClockAccess struct {
	sync.Mutex
	interfaces map[string]*timestampLease
}

func accessForPHC(index int32) *hardwareClockAccess {
	value, _ := hardwareClockAccesses.LoadOrStore(index, &hardwareClockAccess{interfaces: make(map[string]*timestampLease)})
	return value.(*hardwareClockAccess)
}

func timestampConfigCovers(cfg unix.HwTstampConfig, filter int32) bool {
	return cfg.Tx_type == unix.HWTSTAMP_TX_ON && (cfg.Rx_filter == filter || cfg.Rx_filter == unix.HWTSTAMP_FILTER_ALL ||
		cfg.Rx_filter == unix.HWTSTAMP_FILTER_PTP_V2_EVENT &&
			(filter == unix.HWTSTAMP_FILTER_PTP_V2_L2_EVENT || filter == unix.HWTSTAMP_FILTER_PTP_V2_L4_EVENT))
}

func (a *hardwareClockAccess) acquire(iface string, filter int32, get func() (*unix.HwTstampConfig, error), set func(*unix.HwTstampConfig) error) error {
	a.Lock()
	defer a.Unlock()
	if lease := a.interfaces[iface]; lease != nil {
		if !timestampConfigCovers(lease.config, filter) {
			return errors.New("active PTP transport uses an incompatible hardware timestamp filter")
		}
		lease.users++
		return nil
	}
	cfg, err := get()
	if err != nil || cfg == nil || !timestampConfigCovers(*cfg, filter) {
		cfg = &unix.HwTstampConfig{Tx_type: unix.HWTSTAMP_TX_ON, Rx_filter: filter}
		if err := set(cfg); err != nil {
			return err
		}
	}
	// A successful SET may report SOME rather than a reusable filter mask.
	// Preserve support for those drivers, but do not assume that such a mask
	// can satisfy a second transport's request without reconfiguration.
	if cfg.Tx_type != unix.HWTSTAMP_TX_ON || cfg.Rx_filter == unix.HWTSTAMP_FILTER_NONE {
		return errors.New("driver did not enable hardware timestamping")
	}
	a.interfaces[iface] = &timestampLease{config: *cfg, users: 1}
	return nil
}

func (a *hardwareClockAccess) release(iface string) {
	a.Lock()
	defer a.Unlock()
	if lease := a.interfaces[iface]; lease != nil {
		lease.users--
		if lease.users == 0 {
			delete(a.interfaces, iface)
		}
	}
}

func configureHardwareTimestamping(fd int, ifaceName string, rxFilter int32, clockMode timestampClockMode) (*hardwareClockConverter, error) {
	info, err := unix.IoctlGetEthtoolTsInfo(fd, ifaceName)
	if err != nil {
		return nil, fmt.Errorf("%w: query %s: %v", ErrHardwareTimestampingUnavailable, ifaceName, err)
	}
	if info.Phc_index < 0 || info.Tx_types&(1<<unix.HWTSTAMP_TX_ON) == 0 ||
		info.Rx_filters&(1<<uint32(rxFilter)) == 0 &&
			info.Rx_filters&(1<<unix.HWTSTAMP_FILTER_PTP_V2_EVENT) == 0 &&
			info.Rx_filters&(1<<unix.HWTSTAMP_FILTER_ALL) == 0 {
		return nil, fmt.Errorf("%w on %s", ErrHardwareTimestampingUnavailable, ifaceName)
	}

	access := accessForPHC(info.Phc_index)
	if err := access.acquire(ifaceName, rxFilter,
		func() (*unix.HwTstampConfig, error) { return unix.IoctlGetHwTstamp(fd, ifaceName) },
		func(cfg *unix.HwTstampConfig) error { return unix.IoctlSetHwTstamp(fd, ifaceName, cfg) }); err != nil {
		return nil, fmt.Errorf("%w: configure %s: %v", ErrHardwareTimestampingUnavailable, ifaceName, err)
	}

	file, err := os.Open(fmt.Sprintf("/dev/ptp%d", info.Phc_index))
	if err != nil {
		access.release(ifaceName)
		return nil, fmt.Errorf("%w: open PHC for %s: %v", ErrHardwareTimestampingUnavailable, ifaceName, err)
	}
	converter := &hardwareClockConverter{file: file, clockMode: clockMode, access: access, ifaceName: ifaceName}
	if err := converter.initialize(); err != nil {
		converter.Close()
		return nil, fmt.Errorf("%w: read PHC for %s: %v", ErrHardwareTimestampingUnavailable, ifaceName, err)
	}
	return converter, nil
}

func (c *hardwareClockConverter) Close() {
	if c != nil {
		c.closeOnce.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.file != nil {
				_ = c.file.Close()
			}
			if c.access != nil {
				c.access.release(c.ifaceName)
			}
		})
	}
}

func (c *hardwareClockConverter) initialize() error {
	if c.clockMode != timestampClockAutonomous {
		_, err := c.currentOffset()
		return err
	}
	initial, err := c.crossTimestamp()
	if err != nil {
		return fmt.Errorf("autonomous Grandmaster requires PTP_SYS_OFFSET_PRECISE: %w", err)
	}
	clock, err := newAutonomousPHCClock(initial)
	if err != nil {
		return err
	}
	// Establish the PHC/MONOTONIC_RAW rate before the first Sync. The target
	// NIC starts with a sizeable free-running rate error, so a unit-rate first
	// packet would otherwise create an avoidable acquisition transient.
	time.Sleep(initialPHCCalibration)
	second, err := c.crossTimestamp()
	if err != nil {
		return err
	}
	if err := clock.Calibrate(second); err != nil {
		return err
	}
	if !clock.rateValid {
		return errors.New("PHC calibration window was too short")
	}
	c.autonomous = clock
	return nil
}

func (c *hardwareClockConverter) ToClock(raw time.Time) (time.Time, error) {
	if c == nil {
		return time.Time{}, errors.New("PTP hardware clock converter is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.autonomous != nil {
		sample, err := c.crossTimestamp()
		if err != nil {
			return time.Time{}, err
		}
		return c.autonomous.Convert(raw, sample)
	}
	offset, err := c.currentOffsetUnlocked()
	if err != nil {
		return time.Time{}, err
	}
	return raw.Add(offset).UTC(), nil
}

func (c *hardwareClockConverter) Now() (time.Time, error) {
	if c == nil || c.autonomous == nil {
		return time.Now().UTC(), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	monoraw, err := c.monorawTime()
	if err != nil {
		return time.Time{}, err
	}
	return c.autonomous.Now(monoraw), nil
}

func (c *hardwareClockConverter) currentOffset() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentOffsetUnlocked()
}

func (c *hardwareClockConverter) currentOffsetUnlocked() (time.Duration, error) {
	if c.access != nil {
		c.access.Lock()
		defer c.access.Unlock()
	}
	if c.readOffset != nil {
		return c.readOffset()
	}
	return readHardwareClockOffset(int(c.file.Fd()))
}

func (c *hardwareClockConverter) crossTimestamp() (phcCrossTimestamp, error) {
	if c.access != nil {
		c.access.Lock()
		defer c.access.Unlock()
	}
	if c.readCross != nil {
		return c.readCross()
	}
	precise, err := unix.IoctlPtpSysOffsetPrecise(int(c.file.Fd()))
	if err != nil {
		return phcCrossTimestamp{}, err
	}
	return phcCrossTimestamp{
		Device:   ptpClockTime(precise.Device),
		Realtime: ptpClockTime(precise.Realtime),
		Monoraw:  ptpClockTime(precise.Monoraw),
	}, nil
}

func (c *hardwareClockConverter) monorawTime() (time.Time, error) {
	if c.readMonoraw != nil {
		return c.readMonoraw()
	}
	var value unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &value); err != nil {
		return time.Time{}, err
	}
	return time.Unix(value.Sec, value.Nsec).UTC(), nil
}

// readHardwareClockOffset deliberately obtains a fresh PHC-to-CLOCK_REALTIME
// cross-timestamp for every PTP event. A cached scalar offset is only valid
// when both clocks are frequency-disciplined. On a free-running PHC even a
// short cache lifetime turns clock-rate error directly into PTP offset jitter.
func readHardwareClockOffset(fd int) (time.Duration, error) {
	if precise, err := unix.IoctlPtpSysOffsetPrecise(fd); err == nil {
		device := ptpClockTime(precise.Device)
		realtime := ptpClockTime(precise.Realtime)
		return realtime.Sub(device), nil
	}
	if extended, err := unix.IoctlPtpSysOffsetExtended(fd, 5); err == nil {
		bestWidth := time.Duration(1<<63 - 1)
		var bestOffset time.Duration
		for i := 0; i < int(extended.Samples); i++ {
			before := ptpClockTime(extended.Ts[i][0])
			device := ptpClockTime(extended.Ts[i][1])
			after := ptpClockTime(extended.Ts[i][2])
			width := after.Sub(before)
			if width < bestWidth {
				bestWidth = width
				realtime := before.Add(width / 2)
				bestOffset = realtime.Sub(device)
			}
		}
		return bestOffset, nil
	}

	before := time.Now()
	var phc unix.Timespec
	if err := unix.ClockGettime(unix.FdToClockID(fd), &phc); err != nil {
		return 0, err
	}
	after := time.Now()
	realtime := before.Add(after.Sub(before) / 2)
	return realtime.Sub(time.Unix(phc.Sec, phc.Nsec)), nil
}

func ptpClockTime(t unix.PtpClockTime) time.Time {
	return time.Unix(t.Sec, int64(t.Nsec)).UTC()
}

// txTimestampControlMessage requests a TX timestamp only for the current event
// packet. General PTP messages then cannot leave unrelated timestamps in the
// socket error queue.
func txTimestampControlMessage(mode string) []byte {
	var flag uint32
	switch mode {
	case "hardware":
		flag = uint32(unix.SOF_TIMESTAMPING_TX_HARDWARE)
	case "software":
		flag = uint32(unix.SOF_TIMESTAMPING_TX_SOFTWARE)
	default:
		return nil
	}
	oob := make([]byte, unix.CmsgSpace(4))
	header := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
	header.Level = unix.SOL_SOCKET
	header.Type = unix.SO_TIMESTAMPING
	header.SetLen(unix.CmsgLen(4))
	binary.NativeEndian.PutUint32(oob[unix.CmsgLen(0):], flag)
	return oob
}

func extractKernelTimestamp(oob []byte, mode string, converter *hardwareClockConverter) (time.Time, error) {
	if len(oob) == 0 {
		return time.Time{}, timestampMissingError(mode, "RX")
	}
	scms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse RX timestamp metadata: %w", err)
	}
	for _, scm := range scms {
		if scm.Header.Level != syscall.SOL_SOCKET {
			continue
		}
		switch scm.Header.Type {
		case soTimestampNS:
			if mode != "hardware" && len(scm.Data) >= 16 {
				if ts, ok := readTimespec(scm.Data); ok {
					return ts, nil
				}
			}
		case soTimestamping:
			if len(scm.Data) < 16 {
				continue
			}
			if mode == "hardware" && len(scm.Data) >= 48 {
				if ts, ok := readTimespec(scm.Data[32:]); ok {
					converted, err := converter.ToClock(ts)
					if err != nil {
						return time.Time{}, fmt.Errorf("convert RX hardware timestamp: %w", err)
					}
					return converted, nil
				}
			}
			if mode != "hardware" {
				if ts, ok := readTimespec(scm.Data[0:]); ok {
					return ts, nil
				}
			}
		}
	}
	return time.Time{}, timestampMissingError(mode, "RX")
}

func drainTXErrQueue(fd int) {
	buf := make([]byte, 256)
	oob := make([]byte, oobSize)
	for {
		_, _, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return
		}
	}
}

func drainTXErrQueueConn(rawConn syscall.RawConn) {
	fd, err := duplicateRawConnFD(rawConn)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	drainTXErrQueue(fd)
}

func readTXTimestamp(fd int, mode string, converter *hardwareClockConverter) (time.Time, error) {
	buf := make([]byte, 256)
	oob := make([]byte, oobSize)
	deadline := time.Now().Add(txTimestampTimeout)
	for {
		_, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return time.Time{}, fmt.Errorf("%w after %s", timestampMissingError(mode, "TX"), txTimestampTimeout)
			}
			timeoutMS := int((remaining + time.Millisecond - 1) / time.Millisecond)
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLERR}}
			ready, pollErr := unix.Poll(fds, timeoutMS)
			if pollErr == unix.EINTR {
				continue
			}
			if pollErr != nil {
				return time.Time{}, fmt.Errorf("wait for TX %s timestamp: %w", mode, pollErr)
			}
			if ready == 0 {
				return time.Time{}, fmt.Errorf("%w after %s", timestampMissingError(mode, "TX"), txTimestampTimeout)
			}
			continue
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("read TX %s timestamp: %w", mode, err)
		}
		if oobn == 0 {
			continue
		}
		ts, err := extractTXTimestamp(oob[:oobn], mode, converter)
		if err != nil {
			if errors.Is(err, ErrHardwareTimestampMissing) || errors.Is(err, ErrSoftwareTimestampMissing) {
				continue
			}
			return time.Time{}, err
		}
		return ts, nil
	}
}

func readTXTimestampConn(rawConn syscall.RawConn, mode string, converter *hardwareClockConverter) (time.Time, error) {
	fd, err := duplicateRawConnFD(rawConn)
	if err != nil {
		return time.Time{}, err
	}
	defer unix.Close(fd)
	return readTXTimestamp(fd, mode, converter)
}

// duplicateRawConnFD keeps blocking poll/MSG_ERRQUEUE work outside
// syscall.RawConn.Control. The duplicate references the same socket error
// queue and remains valid if the net.UDPConn is closed concurrently.
func duplicateRawConnFD(rawConn syscall.RawConn) (int, error) {
	fd := -1
	var dupErr error
	if err := rawConn.Control(func(rawFD uintptr) {
		fd, dupErr = unix.Dup(int(rawFD))
	}); err != nil {
		return -1, err
	}
	if dupErr != nil {
		return -1, dupErr
	}
	return fd, nil
}

func extractTXTimestamp(oob []byte, mode string, converter *hardwareClockConverter) (time.Time, error) {
	scms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, err
	}
	for _, scm := range scms {
		if scm.Header.Level != syscall.SOL_SOCKET || scm.Header.Type != soTimestamping {
			continue
		}
		if len(scm.Data) < 16 {
			continue
		}
		if mode == "hardware" && len(scm.Data) >= 48 {
			if ts, ok := readTimespec(scm.Data[32:]); ok {
				converted, err := converter.ToClock(ts)
				if err != nil {
					return time.Time{}, fmt.Errorf("convert TX hardware timestamp: %w", err)
				}
				return converted, nil
			}
		}
		if mode != "hardware" {
			if ts, ok := readTimespec(scm.Data[0:]); ok {
				return ts, nil
			}
		}
	}
	return time.Time{}, timestampMissingError(mode, "TX")
}

func readTimespec(b []byte) (time.Time, bool) {
	if len(b) < 16 {
		return time.Time{}, false
	}
	sec := int64(binary.NativeEndian.Uint64(b[0:8]))
	nsec := int64(binary.NativeEndian.Uint64(b[8:16]))
	// An unused SCM_TIMESTAMPING slot is encoded as {0, 0}. time.Unix(0, 0)
	// is not Go's zero value, so the raw fields must be checked first.
	if sec == 0 && nsec == 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, nsec).UTC(), true
}
