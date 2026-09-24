package goose

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// testPcapPath returns the reference pcap path relative to the repository root.
func testPcapPath(t *testing.T) string {
	t.Helper()
	// Repository root: .../<repo>/internal/domain/goose -> .../<repo>.
	root := filepath.Join("..", "..", "..")
	p := filepath.Join(root, "examles", "goose_ receiver", "goose_dump_example.pcap")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("pcap not available: %v", err)
	}
	return p
}

// readPcapFrames is a minimal classic-pcap reader without pcapng support.
func readPcapFrames(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pcap: %v", err)
	}
	if len(data) < 24 {
		t.Fatalf("pcap too short")
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	var le bool
	switch magic {
	case 0xa1b2c3d4, 0xa1b23c4d:
		le = true
	case 0xd4c3b2a1, 0x4d3cb2a1:
		le = false
	default:
		t.Fatalf("unknown pcap magic: 0x%08x", magic)
	}
	u32 := func(b []byte) uint32 {
		if le {
			return binary.LittleEndian.Uint32(b)
		}
		return binary.BigEndian.Uint32(b)
	}
	var frames [][]byte
	off := 24
	for off+16 <= len(data) {
		inclLen := int(u32(data[off+8 : off+12]))
		off += 16
		if off+inclLen > len(data) {
			t.Fatalf("truncated record at %d", off)
		}
		frame := make([]byte, inclLen)
		copy(frame, data[off:off+inclLen])
		frames = append(frames, frame)
		off += inclLen
	}
	return frames
}

func TestDecodeFromExamplePcap(t *testing.T) {
	path := testPcapPath(t)
	frames := readPcapFrames(t, path)
	if len(frames) == 0 {
		t.Fatal("no frames in pcap")
	}

	var decoded int
	dsts := make(map[string]int)
	for _, f := range frames {
		pdu, err := Decode(f)
		if err != nil {
			// Skip non-GOOSE traffic.
			continue
		}
		decoded++
		dsts[pdu.DstMAC.String()]++

		// Basic field validation.
		if pdu.AppID == 0 && pdu.GocbRef == "" {
			t.Errorf("decoded PDU has empty AppID and GocbRef")
		}
		if pdu.TimeAllowedToLiveMs == 0 {
			t.Errorf("TimeAllowedToLive should be > 0")
		}
		if pdu.NumDatSetEntries == 0 {
			t.Errorf("NumDatSetEntries should be > 0")
		}
		if int(pdu.NumDatSetEntries) != len(pdu.AllData) {
			t.Errorf("NumDatSetEntries=%d != len(AllData)=%d", pdu.NumDatSetEntries, len(pdu.AllData))
		}
		if pdu.Timestamp.IsZero() {
			t.Errorf("timestamp not decoded")
		}
	}
	if decoded == 0 {
		t.Fatal("no GOOSE frames decoded from pcap")
	}
	t.Logf("decoded %d GOOSE frames, dst MACs: %v", decoded, dsts)
}

func TestSubscriptionMatch(t *testing.T) {
	// Use a synthetic frame so the subscription is independent of captured devices.
	pdu := strictTestPDU()
	pdu.SrcMAC = mustMAC(t, "02:00:00:00:00:01")
	pdu.GocbRef = "IED1/LLN0$GO$Control"
	pdu.DatSet = "IED1/LLN0$DataSet1"
	pdu.GoID = "CTRL1"
	decoded, err := Decode(Encode(pdu))
	if err != nil {
		t.Fatalf("decode synthetic frame: %v", err)
	}

	sub := &Subscription{
		Name:    "IED_Control",
		DstMAC:  mustMAC(t, "01:0c:cd:01:00:01"),
		AppID:   0x0001,
		GocbRef: "IED1/LLN0$GO$Control",
		GoID:    "CTRL1",
	}

	if !sub.Matches(decoded) {
		t.Fatal("synthetic frame did not match subscription")
	}
	decoded.GocbRef = "IED2/LLN0$GO$Control"
	if sub.Matches(decoded) {
		t.Fatal("frame with a different GoCB reference matched subscription")
	}
}

func mustMAC(t *testing.T, s string) []byte {
	t.Helper()
	// Local parser avoids importing net in the domain test.
	out := make([]byte, 6)
	idx := 0
	var cur byte
	nib := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ':' || c == '-' {
			if nib != 2 {
				t.Fatalf("bad MAC %q", s)
			}
			out[idx] = cur
			idx++
			cur = 0
			nib = 0
			continue
		}
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			t.Fatalf("bad MAC %q", s)
		}
		cur = cur<<4 | v
		nib++
	}
	if nib == 2 {
		out[idx] = cur
		idx++
	}
	if idx != 6 {
		t.Fatalf("bad MAC %q", s)
	}
	return out
}
