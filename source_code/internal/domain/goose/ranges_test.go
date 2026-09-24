package goose

import (
	"net"
	"testing"
)

func TestIsValidMulticastMAC(t *testing.T) {
	cases := []struct {
		mac  string
		want bool
	}{
		{"01:0c:cd:01:00:00", true},  // min
		{"01:0c:cd:01:01:ff", true},  // max
		{"01:0c:cd:01:00:22", true},  // typical
		{"01:0c:cd:01:02:00", false}, // above max
		{"01:0c:cd:02:00:00", false}, // wrong 4th byte
		{"01:0c:cd:00:ff:ff", false}, // below min
		{"ff:ff:ff:ff:ff:ff", false}, // broadcast
		{"00:11:22:33:44:55", false}, // unicast
	}
	for _, c := range cases {
		m, _ := net.ParseMAC(c.mac)
		if got := IsValidMulticastMAC(m); got != c.want {
			t.Errorf("IsValidMulticastMAC(%s) = %v, want %v", c.mac, got, c.want)
		}
	}
}

func TestIsValidAppID(t *testing.T) {
	if !IsValidAppID(0x0000) || !IsValidAppID(0x3FFF) {
		t.Error("boundary AppIDs must be valid")
	}
	if IsValidAppID(0x4000) || IsValidAppID(0xFFFF) {
		t.Error("AppIDs above 0x3FFF must be invalid")
	}
}
