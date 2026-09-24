package ptpclient

import "time"

// TsProc processes PTP timestamps.
// It calculates offset and delay from t1, t2, t3, and t4.
//
// E2E delay measurement:
//
//	delay = ((t2-t1) + (t4-t3)) / 2
//	offset = (t2-t1) - delay = ((t2-t1) - (t4-t3)) / 2
//
// P2P peer delay:
//
//	peerDelay = ((t2-t1) + (t4-t3)) / 2  (from the pdelay exchange)
//	offset = (syncRx - syncTx) - peerDelay  (correctionField is handled separately)
type TsProc struct {
	t1 time.Time // master TX (sync origin or follow_up precise origin)
	t2 time.Time // slave RX (sync receive timestamp)
	t3 time.Time // slave TX (delay_req send timestamp)
	t4 time.Time // master RX (delay_resp receive timestamp)

	peerDelay time.Duration // calculated peer delay for P2P

	correctionSync      int64 // correctionField from Sync (ns × 2^16)
	correctionFollowUp  int64 // correctionField from FollowUp
	correctionDelayResp int64 // correctionField from DelayResp
}

// SetT1 sets the master TX timestamp.
func (p *TsProc) SetT1(t time.Time) { p.t1 = t }

// SetT2 sets the slave RX timestamp.
func (p *TsProc) SetT2(t time.Time) { p.t2 = t }

// SetT3 sets the slave TX timestamp (delay_req).
func (p *TsProc) SetT3(t time.Time) { p.t3 = t }

// SetT4 sets the master RX timestamp (delay_resp).
func (p *TsProc) SetT4(t time.Time) { p.t4 = t }

// SetPeerDelay sets the peer delay for P2P.
func (p *TsProc) SetPeerDelay(d time.Duration) { p.peerDelay = d }

// SetCorrectionSync sets correctionField from Sync.
func (p *TsProc) SetCorrectionSync(cf int64) { p.correctionSync = cf }

// SetCorrectionFollowUp sets correctionField from FollowUp.
func (p *TsProc) SetCorrectionFollowUp(cf int64) { p.correctionFollowUp = cf }

// SetCorrectionDelayResp sets correctionField from DelayResp.
func (p *TsProc) SetCorrectionDelayResp(cf int64) { p.correctionDelayResp = cf }

// correctionDuration converts correctionField units (2^-16 ns) to a Go
// duration. Division, unlike a signed right shift, truncates negative
// fractions towards zero.
func correctionDuration(cf int64) time.Duration {
	return time.Duration(cf / (1 << 16))
}

// OffsetE2E calculates the offset for the E2E delay mechanism.
//
//	delay  = (d21 + d43 - syncCorrection - delayCorrection) / 2
//	offset = (d21 - d43 - syncCorrection + delayCorrection) / 2
//
// where syncCorrection is the sum of Sync and FollowUp correctionField,
// and delayCorrection is DelayResp correctionField.
// All four timestamps must be set.
func (p *TsProc) OffsetE2E() (offset time.Duration, delay time.Duration, ok bool) {
	if p.t1.IsZero() || p.t2.IsZero() || p.t3.IsZero() || p.t4.IsZero() {
		return 0, 0, false
	}
	d21 := p.t2.Sub(p.t1) // t2-t1
	d43 := p.t4.Sub(p.t3) // t4-t3

	syncCorrection := correctionDuration(p.correctionSync + p.correctionFollowUp)
	delayCorrection := correctionDuration(p.correctionDelayResp)

	delay = (d21 + d43 - syncCorrection - delayCorrection) / 2
	offset = (d21 - d43 - syncCorrection + delayCorrection) / 2
	return offset, delay, true
}

// OffsetP2P calculates the offset for the P2P delay mechanism.
// offset = (t2 - t1) - peerDelay - correction
func (p *TsProc) OffsetP2P() (offset time.Duration, ok bool) {
	if p.t1.IsZero() || p.t2.IsZero() {
		return 0, false
	}
	d21 := p.t2.Sub(p.t1)

	syncCorrection := correctionDuration(p.correctionSync + p.correctionFollowUp)
	offset = d21 - p.peerDelay - syncCorrection
	return offset, true
}

// Reset clears all timestamps.
func (p *TsProc) Reset() {
	p.t1 = time.Time{}
	p.t2 = time.Time{}
	p.t3 = time.Time{}
	p.t4 = time.Time{}
	p.correctionSync = 0
	p.correctionFollowUp = 0
	p.correctionDelayResp = 0
}
