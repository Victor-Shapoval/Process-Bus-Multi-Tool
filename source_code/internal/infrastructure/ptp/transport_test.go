package ptp

import (
	"net"
	"testing"

	ptpdomain "pbmt/internal/domain/ptp"
)

func TestIsPDelayEvent(t *testing.T) {
	pdelay := ptpdomain.EncodePDelayReq(ptpdomain.Header{MessageType: ptpdomain.MsgPDelayReq}, ptpdomain.PDelayReqBody{})
	if !isPDelayEvent(pdelay) {
		t.Fatal("PDelayReq must use the peer-delay multicast destination")
	}
	sync := ptpdomain.EncodeSync(ptpdomain.Header{MessageType: ptpdomain.MsgSync}, ptpdomain.SyncBody{})
	if isPDelayEvent(sync) {
		t.Fatal("Sync must use the primary multicast destination")
	}
}

func TestIsPDelayGeneral(t *testing.T) {
	pdelayFollowUp := ptpdomain.EncodePDelayRespFollowUp(
		ptpdomain.Header{MessageType: ptpdomain.MsgPDelayRespFollowUp},
		ptpdomain.PDelayRespFollowUpBody{},
	)
	if !isPDelayGeneral(pdelayFollowUp) {
		t.Fatal("PDelayRespFollowUp must use the peer-delay multicast destination")
	}
	followUp := ptpdomain.EncodeFollowUp(ptpdomain.Header{MessageType: ptpdomain.MsgFollowUp}, ptpdomain.FollowUpBody{})
	if isPDelayGeneral(followUp) {
		t.Fatal("FollowUp must use the primary multicast destination")
	}
}

func TestUDPGeneralDestinationUsesGeneralPort(t *testing.T) {
	source := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: ptpdomain.UDPEventPort}
	dst, err := udpGeneralDestination(source)
	if err != nil {
		t.Fatal(err)
	}
	if dst.Port != ptpdomain.UDPGeneralPort {
		t.Fatalf("destination port: want %d, got %d", ptpdomain.UDPGeneralPort, dst.Port)
	}
	if source.Port != ptpdomain.UDPEventPort {
		t.Fatal("source address was mutated")
	}
}
