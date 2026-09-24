package sv

import "net"

// Subscription describes a subscription to one SV stream.
// Empty strings and nil fields mean "do not filter." APPID, including 0,
// is matched exactly; MatchAnyAppID explicitly disables this filter.
type Subscription struct {
	Name                  string           // human-readable name
	DstMAC                net.HardwareAddr // destination multicast MAC (required)
	SrcMAC                net.HardwareAddr // source MAC (nil matches any)
	AppID                 uint16           // exact value, including 0
	MatchAnyAppID         bool             // true disables APPID filtering
	SvID                  string           // empty matches any
	VLANID                *uint16          // nil disables filtering
	SmpRate               uint16           // samples per period for calculations when the ASDU omits smpRate
	SampleTimingFrequency uint16           // nominal grid frequency in Hz (50 or 60)
	BaseVector            string           // reference vector for frequency estimation from phase drift
}

// Matches reports whether an ASDU meets the subscription criteria.
func (s *Subscription) Matches(a *ASDU) bool {
	if a == nil {
		return false
	}
	if !macEqual(s.DstMAC, a.DstMAC) {
		return false
	}
	if s.SrcMAC != nil && !macEqual(s.SrcMAC, a.SrcMAC) {
		return false
	}
	if !s.MatchAnyAppID && s.AppID != a.AppID {
		return false
	}
	if s.SvID != "" && s.SvID != a.SvID {
		return false
	}
	if s.VLANID != nil {
		if a.VLAN == nil || a.VLAN.VID != *s.VLANID {
			return false
		}
	}
	return true
}

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
