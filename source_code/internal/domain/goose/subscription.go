package goose

import (
	"net"
)

// Subscription describes one subscribed GOOSE Control Block.
// Empty strings and nil pointers mean "do not filter." APPID, including zero,
// is matched exactly; MatchAnyAppID explicitly disables that filter.
// DstMAC is required. Test, Simulation, and ndsCom are rejected by default and
// must be explicitly accepted for commissioning and diagnostics.
type Subscription struct {
	Name             string           // human-readable name for logs and events
	DstMAC           net.HardwareAddr // required destination multicast MAC
	SrcMAC           net.HardwareAddr // source MAC; nil matches any source
	AppID            uint16           // exact value, including zero
	MatchAnyAppID    bool             // true disables APPID filtering
	GocbRef          string           // empty matches any value
	DatSet           string           // empty matches any value
	GoID             string           // empty matches any value
	ConfRev          *uint32          // nil disables filtering
	VLANID           *uint16          // nil disables filtering
	VLANPriority     *uint8           // nil disables filtering
	AcceptTest       bool             // accept test=TRUE only during commissioning
	AcceptSimulation bool             // accept frames with the Simulation bit set
	AcceptNdsCom     bool             // diagnostic option; ndsCom=TRUE is normally ignored
}

// Matches reports whether a PDU satisfies the subscription criteria.
func (s *Subscription) Matches(pdu *PDU) bool {
	if pdu == nil {
		return false
	}
	if !macEqual(s.DstMAC, pdu.DstMAC) {
		return false
	}
	if s.SrcMAC != nil && !macEqual(s.SrcMAC, pdu.SrcMAC) {
		return false
	}
	if !s.MatchAnyAppID && s.AppID != pdu.AppID {
		return false
	}
	if s.GocbRef != "" && s.GocbRef != pdu.GocbRef {
		return false
	}
	if s.DatSet != "" && s.DatSet != pdu.DatSet {
		return false
	}
	if s.GoID != "" && s.GoID != pdu.GoID {
		return false
	}
	if s.ConfRev != nil && *s.ConfRev != pdu.ConfRev {
		return false
	}
	if s.VLANID != nil {
		if pdu.VLAN == nil || pdu.VLAN.VID != *s.VLANID {
			return false
		}
	}
	if s.VLANPriority != nil {
		if pdu.VLAN == nil || pdu.VLAN.Priority != *s.VLANPriority {
			return false
		}
	}
	if pdu.Test && !s.AcceptTest {
		return false
	}
	if pdu.Simulation && !s.AcceptSimulation {
		return false
	}
	if pdu.NdsCom && !s.AcceptNdsCom {
		return false
	}
	return true
}

// macEqual compares two MAC addresses case-insensitively.
func macEqual(a, b net.HardwareAddr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
