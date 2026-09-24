// Package inject provides a libpcap adapter for injecting raw Ethernet frames.
package inject

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/gopacket/pcap"
)

// Injector sends raw Ethernet frames through pcap_inject.
type Injector struct {
	handle *pcap.Handle
	mu     sync.Mutex
	closed bool
}

// Config configures the injector.
type Config struct {
	Interface string // network interface (required)
}

// Open opens a pcap handle for frame injection.
func Open(cfg Config) (*Injector, error) {
	if cfg.Interface == "" {
		return nil, errors.New("inject: Interface is required")
	}

	handle, err := pcap.OpenLive(cfg.Interface, 65535, false, pcap.BlockForever)
	if err != nil {
		return nil, fmt.Errorf("inject: open %q: %w (hint: run with sudo/CAP_NET_RAW)", cfg.Interface, err)
	}

	return &Injector{handle: handle}, nil
}

// WriteFrame sends a raw Ethernet frame over the network.
func (inj *Injector) WriteFrame(frame []byte) error {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if inj.closed {
		return errors.New("inject: closed")
	}
	return inj.handle.WritePacketData(frame)
}

// Close closes the pcap handle.
func (inj *Injector) Close() error {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if !inj.closed {
		inj.closed = true
		inj.handle.Close()
	}
	return nil
}
