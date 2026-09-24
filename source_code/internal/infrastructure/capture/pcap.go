// Package capture provides a libpcap adapter for live Ethernet frame capture.
// It implements the ethernet.Source port for GOOSE and SV subscribers.
package capture

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/gopacket/pcap"

	"pbmt/internal/domain/ethernet"
)

// Config configures live capture through libpcap.
type Config struct {
	Interface   string        // network interface for live capture (required)
	BPFFilter   string        // BPF filter; empty means no filter
	SnapLen     int32         // snapshot length; 0 uses 65535
	Promiscuous bool          // promiscuous mode
	Immediate   bool          // SetImmediateMode, recommended for low latency
	Timeout     time.Duration // read timeout; 0 uses 1 s
	ChanBuffer  int           // Frames channel capacity; 0 uses 256
}

// PcapSource implements ethernet.Source through libpcap.
type PcapSource struct {
	handle *pcap.Handle
	frames chan ethernet.Frame
	errs   chan error
	done   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// Open opens a live interface, applies the BPF filter, and starts a background
// goroutine that reads packets.
func Open(cfg Config) (*PcapSource, error) {
	if cfg.Interface == "" {
		return nil, errors.New("capture: Interface is required")
	}
	snap := cfg.SnapLen
	if snap <= 0 {
		snap = 65535
	}
	chanBuf := cfg.ChanBuffer
	if chanBuf <= 0 {
		chanBuf = 256
	}

	handle, err := openLive(cfg, snap)
	if err != nil {
		return nil, err
	}

	if cfg.BPFFilter != "" {
		if err := handle.SetBPFFilter(cfg.BPFFilter); err != nil {
			handle.Close()
			return nil, fmt.Errorf("capture: BPF filter %q: %w", cfg.BPFFilter, err)
		}
	}

	s := &PcapSource{
		handle: handle,
		frames: make(chan ethernet.Frame, chanBuf),
		errs:   make(chan error, 16),
		done:   make(chan struct{}),
	}

	s.wg.Add(1)
	go s.readLoop()
	return s, nil
}

// openLive opens a live interface through InactiveHandle for full control of
// capture parameters, particularly SetImmediateMode.
func openLive(cfg Config, snap int32) (*pcap.Handle, error) {
	inactive, err := pcap.NewInactiveHandle(cfg.Interface)
	if err != nil {
		return nil, fmt.Errorf("capture: inactive handle for %q: %w", cfg.Interface, err)
	}
	defer inactive.CleanUp()

	if err := inactive.SetSnapLen(int(snap)); err != nil {
		return nil, fmt.Errorf("capture: SetSnapLen: %w", err)
	}
	if err := inactive.SetPromisc(cfg.Promiscuous); err != nil {
		return nil, fmt.Errorf("capture: SetPromisc: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = time.Second
	}
	if err := inactive.SetTimeout(timeout); err != nil {
		return nil, fmt.Errorf("capture: SetTimeout: %w", err)
	}
	if cfg.Immediate {
		if err := inactive.SetImmediateMode(true); err != nil {
			return nil, fmt.Errorf("capture: SetImmediateMode: %w", err)
		}
	}
	handle, err := inactive.Activate()
	if err != nil {
		return nil, fmt.Errorf("capture: activate %q: %w (hint: run with sudo/CAP_NET_RAW)", cfg.Interface, err)
	}
	return handle, nil
}

// Frames returns the channel of received frames.
func (s *PcapSource) Frames() <-chan ethernet.Frame { return s.frames }

// Errors returns the channel of non-blocking capture errors.
func (s *PcapSource) Errors() <-chan error { return s.errs }

// Close stops capture and closes the channels.
func (s *PcapSource) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.handle.Close()
	})
	s.wg.Wait()
	return nil
}

// readLoop reads packets and sends them to the frames channel.
// Packet bytes are copied because libpcap reuses its internal buffer.
func (s *PcapSource) readLoop() {
	defer s.wg.Done()
	defer close(s.frames)
	defer close(s.errs)

	for {
		raw, ci, err := s.handle.ReadPacketData()
		observedAt := time.Now()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if errors.Is(err, pcap.NextErrorTimeoutExpired) {
				continue
			}
			// Non-blocking notification: drop the error if the channel is full.
			select {
			case s.errs <- err:
			default:
			}
			return
		}

		// libpcap reuses the raw buffer, so copy it.
		data := make([]byte, len(raw))
		copy(data, raw)

		frame := ethernet.Frame{
			Data:       data,
			Timestamp:  ci.Timestamp,
			ObservedAt: observedAt,
		}
		select {
		case <-s.done:
			return
		case s.frames <- frame:
		}
	}
}
