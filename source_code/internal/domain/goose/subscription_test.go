package goose

import (
	"net"
	"testing"

	"pbmt/internal/domain/ethernet"
)

func TestSubscriptionMatchesCompleteGooseIdentity(t *testing.T) {
	vlanID := uint16(100)
	vlanPriority := uint8(4)
	confRev := uint32(3)
	sub := &Subscription{
		Name:         "trip",
		DstMAC:       subscriptionTestMAC(t, "01:0c:cd:01:00:01"),
		SrcMAC:       subscriptionTestMAC(t, "00:11:22:33:44:55"),
		AppID:        0x0001,
		GocbRef:      "IED1/LLN0$GO$Control",
		DatSet:       "IED1/LLN0$DataSet1",
		GoID:         "CTRL1",
		ConfRev:      &confRev,
		VLANID:       &vlanID,
		VLANPriority: &vlanPriority,
	}
	pdu := subscriptionTestPDU(t)
	if !sub.Matches(pdu) {
		t.Fatal("complete matching GOOSE identity was rejected")
	}

	tests := []struct {
		name   string
		mutate func(*PDU)
	}{
		{"source MAC", func(p *PDU) { p.SrcMAC[5]++ }},
		{"APPID", func(p *PDU) { p.AppID++ }},
		{"GoCB reference", func(p *PDU) { p.GocbRef += "X" }},
		{"DataSet", func(p *PDU) { p.DatSet += "X" }},
		{"GoID", func(p *PDU) { p.GoID += "X" }},
		{"confRev", func(p *PDU) { p.ConfRev++ }},
		{"VLAN ID", func(p *PDU) { p.VLAN.VID++ }},
		{"VLAN priority", func(p *PDU) { p.VLAN.Priority++ }},
		{"missing VLAN", func(p *PDU) { p.VLAN = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := subscriptionTestPDU(t)
			tc.mutate(candidate)
			if sub.Matches(candidate) {
				t.Fatalf("subscription accepted mismatching %s", tc.name)
			}
		})
	}
}

func TestSubscriptionRejectsNonOperationalMessagesByDefault(t *testing.T) {
	pdu := subscriptionTestPDU(t)
	sub := &Subscription{DstMAC: append(net.HardwareAddr(nil), pdu.DstMAC...), MatchAnyAppID: true}

	tests := []struct {
		name   string
		mutate func(*PDU)
	}{
		{"test", func(p *PDU) { p.Test = true }},
		{"simulation", func(p *PDU) { p.Simulation = true }},
		{"needs commissioning", func(p *PDU) { p.NdsCom = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := subscriptionTestPDU(t)
			tc.mutate(candidate)
			if sub.Matches(candidate) {
				t.Fatalf("subscription accepted %s message by default", tc.name)
			}
		})
	}

	sub.AcceptTest = true
	sub.AcceptSimulation = true
	sub.AcceptNdsCom = true
	pdu.Test = true
	pdu.Simulation = true
	pdu.NdsCom = true
	if !sub.Matches(pdu) {
		t.Fatal("explicit diagnostic acceptance flags were ignored")
	}
}

func TestSubscriptionTreatsZeroAppIDAsExactValue(t *testing.T) {
	pdu := subscriptionTestPDU(t)
	sub := &Subscription{DstMAC: append(net.HardwareAddr(nil), pdu.DstMAC...), AppID: 0}
	if sub.Matches(pdu) {
		t.Fatal("APPID 0 unexpectedly disabled the filter")
	}
	pdu.AppID = 0
	if !sub.Matches(pdu) {
		t.Fatal("subscription rejected exact APPID 0")
	}
	sub.MatchAnyAppID = true
	pdu.AppID = 0x1234
	if !sub.Matches(pdu) {
		t.Fatal("explicit APPID wildcard was ignored")
	}
}

func subscriptionTestPDU(t *testing.T) *PDU {
	t.Helper()
	return &PDU{
		DstMAC:  subscriptionTestMAC(t, "01:0c:cd:01:00:01"),
		SrcMAC:  subscriptionTestMAC(t, "00:11:22:33:44:55"),
		VLAN:    &ethernet.VLANTag{VID: 100, Priority: 4},
		AppID:   0x0001,
		GocbRef: "IED1/LLN0$GO$Control",
		DatSet:  "IED1/LLN0$DataSet1",
		GoID:    "CTRL1",
		ConfRev: 3,
	}
}

func subscriptionTestMAC(t *testing.T, value string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(value)
	if err != nil {
		t.Fatal(err)
	}
	return mac
}
