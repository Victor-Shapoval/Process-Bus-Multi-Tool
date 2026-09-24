package sv

import (
	"net"
	"testing"
)

func TestSubscriptionTreatsZeroAppIDAsExactValue(t *testing.T) {
	dst, err := net.ParseMAC("01:0c:cd:04:00:01")
	if err != nil {
		t.Fatal(err)
	}
	sub := &Subscription{DstMAC: dst, AppID: 0}
	asdu := &ASDU{DstMAC: append(net.HardwareAddr(nil), dst...), AppID: 0x4001}
	if sub.Matches(asdu) {
		t.Fatal("APPID 0 unexpectedly disabled the filter")
	}
	asdu.AppID = 0
	if !sub.Matches(asdu) {
		t.Fatal("subscription rejected exact APPID 0")
	}
	sub.MatchAnyAppID = true
	asdu.AppID = 0x4001
	if !sub.Matches(asdu) {
		t.Fatal("explicit APPID wildcard was ignored")
	}
}
