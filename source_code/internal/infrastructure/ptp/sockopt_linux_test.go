//go:build linux

package ptp

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestSharedHardwareTimestampConfigurationIsNotReapplied(t *testing.T) {
	a := &hardwareClockAccess{interfaces: make(map[string]*timestampLease)}
	gets, sets := 0, 0
	get := func() (*unix.HwTstampConfig, error) {
		gets++
		return &unix.HwTstampConfig{}, nil
	}
	set := func(cfg *unix.HwTstampConfig) error {
		sets++
		cfg.Rx_filter = unix.HWTSTAMP_FILTER_PTP_V2_EVENT
		return nil
	}
	for _, filter := range []int32{unix.HWTSTAMP_FILTER_PTP_V2_L2_EVENT, unix.HWTSTAMP_FILTER_PTP_V2_L4_EVENT} {
		if err := a.acquire("eth0", filter, get, set); err != nil {
			t.Fatal(err)
		}
	}
	if gets != 1 || sets != 1 || a.interfaces["eth0"].users != 2 {
		t.Fatalf("second transport reconfigured NIC: gets=%d sets=%d", gets, sets)
	}
	a.release("eth0")
	if a.interfaces["eth0"].users != 1 {
		t.Fatal("closing one transport released another's lease")
	}
	a.release("eth0")
	if len(a.interfaces) != 0 {
		t.Fatal("last close leaked configuration lease")
	}
}

func TestExistingCompatibleHardwareConfigurationIsReused(t *testing.T) {
	a := &hardwareClockAccess{interfaces: make(map[string]*timestampLease)}
	err := a.acquire("eth0", unix.HWTSTAMP_FILTER_PTP_V2_L2_EVENT,
		func() (*unix.HwTstampConfig, error) {
			return &unix.HwTstampConfig{Tx_type: unix.HWTSTAMP_TX_ON, Rx_filter: unix.HWTSTAMP_FILTER_ALL}, nil
		}, func(*unix.HwTstampConfig) error { t.Fatal("compatible settings were rewritten"); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestIncompatibleActiveTimestampFilterIsNotOverwritten(t *testing.T) {
	a := &hardwareClockAccess{interfaces: map[string]*timestampLease{
		"eth0": {config: unix.HwTstampConfig{Tx_type: unix.HWTSTAMP_TX_ON, Rx_filter: unix.HWTSTAMP_FILTER_PTP_V2_L2_EVENT}, users: 1},
	}}
	err := a.acquire("eth0", unix.HWTSTAMP_FILTER_PTP_V2_L4_EVENT,
		func() (*unix.HwTstampConfig, error) { t.Fatal("unexpected reconfiguration"); return nil, nil },
		func(*unix.HwTstampConfig) error { t.Fatal("active filter overwritten"); return nil })
	if err == nil || a.interfaces["eth0"].users != 1 {
		t.Fatal("incompatible filter accepted")
	}
}

func TestClientAndServerSerializeCrossTimestampReads(t *testing.T) {
	a := accessForPHC(123456)
	if a != accessForPHC(123456) || a == accessForPHC(123457) {
		t.Fatal("PHC access locks are not keyed by hardware clock")
	}
	var active atomic.Int32
	var overlap atomic.Bool
	read := func() {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		time.Sleep(100 * time.Microsecond)
		active.Add(-1)
	}
	client := &hardwareClockConverter{access: a, readOffset: func() (time.Duration, error) { read(); return 0, nil }}
	server := &hardwareClockConverter{access: a, readCross: func() (phcCrossTimestamp, error) { read(); return phcCrossTimestamp{}, nil }}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = client.currentOffset() }()
		go func() { defer wg.Done(); _, _ = server.crossTimestamp() }()
	}
	wg.Wait()
	if overlap.Load() {
		t.Fatal("client and server read one PHC concurrently")
	}
}

func TestHardwareClockConverterReadsFreshOffsetForEveryTimestamp(t *testing.T) {
	offsets := []time.Duration{100 * time.Nanosecond, 250 * time.Nanosecond}
	read := 0
	converter := &hardwareClockConverter{
		readOffset: func() (time.Duration, error) {
			offset := offsets[read]
			read++
			return offset, nil
		},
	}

	raw := time.Unix(100, 500).UTC()
	first, err := converter.ToClock(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := converter.ToClock(raw)
	if err != nil {
		t.Fatal(err)
	}

	if read != 2 {
		t.Fatalf("cross-timestamp reads: want 2, got %d", read)
	}
	if want := raw.Add(offsets[0]); !first.Equal(want) {
		t.Fatalf("first converted timestamp: want %v, got %v", want, first)
	}
	if want := raw.Add(offsets[1]); !second.Equal(want) {
		t.Fatalf("second converted timestamp: want %v, got %v", want, second)
	}
}

func TestHardwareClockConverterFailsClosedWhenCrossTimestampFails(t *testing.T) {
	converter := &hardwareClockConverter{
		readOffset: func() (time.Duration, error) {
			return 0, errors.New("cross-timestamp unavailable")
		},
	}
	if _, err := converter.ToClock(time.Unix(100, 0)); err == nil {
		t.Fatal("expected clock conversion error")
	}
}

func TestHardwareClockConverterUsesAutonomousClock(t *testing.T) {
	epoch := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	initial := phcCrossTimestamp{
		Device: time.Unix(10, 0), Realtime: epoch, Monoraw: time.Unix(100, 0),
	}
	clock, err := newAutonomousPHCClock(initial)
	if err != nil {
		t.Fatal(err)
	}
	sample := phcCrossTimestamp{
		Device: initial.Device.Add(time.Second), Realtime: initial.Realtime.Add(time.Second), Monoraw: initial.Monoraw.Add(time.Second),
	}
	converter := &hardwareClockConverter{
		clockMode:  timestampClockAutonomous,
		autonomous: clock,
		readCross: func() (phcCrossTimestamp, error) {
			return sample, nil
		},
		readMonoraw: func() (time.Time, error) {
			return sample.Monoraw, nil
		},
	}

	converted, err := converter.ToClock(sample.Device)
	if err != nil {
		t.Fatal(err)
	}
	now, err := converter.Now()
	if err != nil {
		t.Fatal(err)
	}
	want := epoch.Add(time.Second)
	if !converted.Equal(want) || !now.Equal(want) {
		t.Fatalf("clock domains differ: converted=%v now=%v want=%v", converted, now, want)
	}
}

func TestTimestampModesRejectMissingRXTimestamp(t *testing.T) {
	if _, err := extractKernelTimestamp(nil, "hardware", &hardwareClockConverter{}); !errors.Is(err, ErrHardwareTimestampMissing) {
		t.Fatalf("expected missing hardware timestamp, got %v", err)
	}
	if _, err := extractKernelTimestamp(nil, "software", nil); !errors.Is(err, ErrSoftwareTimestampMissing) {
		t.Fatalf("expected missing software timestamp, got %v", err)
	}
}

func TestTimestampModesRejectZeroTimestampSlot(t *testing.T) {
	oob := make([]byte, unix.CmsgSpace(48))
	header := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
	header.Level = unix.SOL_SOCKET
	header.Type = unix.SO_TIMESTAMPING
	header.SetLen(unix.CmsgLen(48))

	if _, err := extractKernelTimestamp(oob, "hardware", &hardwareClockConverter{}); !errors.Is(err, ErrHardwareTimestampMissing) {
		t.Fatalf("expected missing raw timestamp for zero slot, got %v", err)
	}
	if _, err := extractKernelTimestamp(oob, "software", nil); !errors.Is(err, ErrSoftwareTimestampMissing) {
		t.Fatalf("expected missing software timestamp for zero slot, got %v", err)
	}
	if _, err := extractTXTimestamp(oob, "software", nil); !errors.Is(err, ErrSoftwareTimestampMissing) {
		t.Fatalf("expected missing software TX timestamp for zero slot, got %v", err)
	}
}

func TestTXTimestampControlMessageRequestsOnlyCurrentPacket(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want uint32
	}{
		{mode: "hardware", want: uint32(unix.SOF_TIMESTAMPING_TX_HARDWARE)},
		{mode: "software", want: uint32(unix.SOF_TIMESTAMPING_TX_SOFTWARE)},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			messages, err := unix.ParseSocketControlMessage(txTimestampControlMessage(tc.mode))
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 {
				t.Fatalf("control messages: want 1, got %d", len(messages))
			}
			message := messages[0]
			if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SO_TIMESTAMPING {
				t.Fatalf("unexpected control header: %+v", message.Header)
			}
			if len(message.Data) < 4 || binary.NativeEndian.Uint32(message.Data[:4]) != tc.want {
				t.Fatalf("unexpected timestamp request payload: % X", message.Data)
			}
		})
	}
	if got := txTimestampControlMessage("unknown"); got != nil {
		t.Fatalf("unknown timestamp mode produced control data: % X", got)
	}
}

func TestGlobalTimestampFlagsDoNotRequestTXForGeneralPackets(t *testing.T) {
	const txGeneration = unix.SOF_TIMESTAMPING_TX_SOFTWARE | unix.SOF_TIMESTAMPING_TX_HARDWARE
	if flags := sofTimestampingRxSW | sofTimestampingReportingOption; flags&txGeneration != 0 {
		t.Fatalf("software socket flags globally request TX timestamps: 0x%X", flags)
	}
	if flags := sofTimestampingRxHW | sofTimestampingReportingOption; flags&txGeneration != 0 {
		t.Fatalf("hardware socket flags globally request TX timestamps: 0x%X", flags)
	}
}

func TestTXFailureIsLatchedForEveryTimestampMode(t *testing.T) {
	first := errors.New("timestamp timeout")
	second := errors.New("later error")

	for _, mode := range []string{"hardware", "software"} {
		t.Run(mode, func(t *testing.T) {
			ethernet := &EthernetTransport{timestampMode: mode, errors: make(chan error, 2)}
			gotFirst := ethernet.recordTXFailure(first)
			gotSecond := ethernet.recordTXFailure(second)
			if gotFirst != gotSecond || !errors.Is(gotFirst, first) {
				t.Fatalf("Ethernet failure was not latched: first=%v second=%v", gotFirst, gotSecond)
			}
			if got := len(ethernet.errors); got != 0 {
				t.Fatalf("synchronous Ethernet error was also reported asynchronously: %d", got)
			}

			udp := &UDPTransport{timestampMode: mode, errors: make(chan error, 2)}
			gotFirst = udp.recordTXFailure(first)
			gotSecond = udp.recordTXFailure(second)
			if gotFirst != gotSecond || !errors.Is(gotFirst, first) {
				t.Fatalf("UDP failure was not latched: first=%v second=%v", gotFirst, gotSecond)
			}
			if got := len(udp.errors); got != 0 {
				t.Fatalf("synchronous UDP error was also reported asynchronously: %d", got)
			}
		})
	}
}
