package ptpclient

import (
	"bytes"
	"time"

	"pbmt/internal/domain/ptp"
)

const (
	foreignMasterThreshold  = 2
	foreignMasterTimeWindow = 4
)

// ForeignMaster is a foreign master record (IEEE 1588-2008, Section 9.3.2.4.4).
type ForeignMaster struct {
	Identity      ptp.PortIdentity
	Announce      ptp.AnnounceBody
	Header        ptp.Header
	ProfileTLVs   ptp.AnnounceProfileTLVs
	LastSeen      time.Time
	AnnounceCount int
	UTCOffset     int16

	announceTimes  []time.Time
	lastSequenceID uint16
	sequenceValid  bool
}

// recordAnnounce records a distinct Announce in the qualification window.
// A foreign master becomes eligible for BMCA only after the threshold is met.
func (m *ForeignMaster) recordAnnounce(now time.Time, window time.Duration, sequenceID uint16) bool {
	cutoff := now.Add(-window)
	kept := m.announceTimes[:0]
	for _, receivedAt := range m.announceTimes {
		if !receivedAt.Before(cutoff) {
			kept = append(kept, receivedAt)
		}
	}
	m.announceTimes = kept
	if m.sequenceValid && len(m.announceTimes) != 0 && sequenceID == m.lastSequenceID {
		m.AnnounceCount = len(m.announceTimes)
		return false
	}
	m.announceTimes = append(m.announceTimes, now)
	m.lastSequenceID = sequenceID
	m.sequenceValid = true
	m.AnnounceCount = len(m.announceTimes)
	m.LastSeen = now
	return true
}

func (m *ForeignMaster) qualified() bool {
	return m != nil && m.AnnounceCount >= foreignMasterThreshold
}

// Dataset contains data used for BMCA comparison (IEEE 1588-2008, Section 9.3.4).
type Dataset struct {
	Priority1    uint8
	Identity     ptp.ClockIdentity
	Quality      ptp.ClockQuality
	Priority2    uint8
	StepsRemoved uint16
	Sender       ptp.PortIdentity
	Receiver     ptp.PortIdentity
}

// DatasetFromAnnounce creates a Dataset from Announce.
func DatasetFromAnnounce(h ptp.Header, a ptp.AnnounceBody, receiverPort ptp.PortIdentity) Dataset {
	return Dataset{
		Priority1:    a.GrandmasterPriority1,
		Identity:     a.GrandmasterIdentity,
		Quality:      a.GrandmasterClockQuality,
		Priority2:    a.GrandmasterPriority2,
		StepsRemoved: a.StepsRemoved,
		Sender:       h.SourcePortIdentity,
		Receiver:     receiverPort,
	}
}

// DSCmp compares two Datasets using BMCA (IEEE 1588-2008, Section 9.3.4).
// It returns < 0 if a is better, > 0 if b is better, and 0 if they are equal.
func DSCmp(a, b *Dataset) int {
	if a == nil && b == nil {
		return 0
	}
	if a != nil && b == nil {
		return -1
	}
	if a == nil && b != nil {
		return 1
	}

	// Check whether both datasets refer to the same grandmaster.
	cmpID := bytes.Compare(a.Identity[:], b.Identity[:])
	if cmpID != 0 {
		// Different grandmasters: compare priority1, quality, priority2, and identity.
		if a.Priority1 != b.Priority1 {
			return int(a.Priority1) - int(b.Priority1)
		}
		if a.Quality.ClockClass != b.Quality.ClockClass {
			return int(a.Quality.ClockClass) - int(b.Quality.ClockClass)
		}
		if a.Quality.ClockAccuracy != b.Quality.ClockAccuracy {
			return int(a.Quality.ClockAccuracy) - int(b.Quality.ClockAccuracy)
		}
		if a.Quality.OffsetScaledLogVariance != b.Quality.OffsetScaledLogVariance {
			return int(a.Quality.OffsetScaledLogVariance) - int(b.Quality.OffsetScaledLogVariance)
		}
		if a.Priority2 != b.Priority2 {
			return int(a.Priority2) - int(b.Priority2)
		}
		return cmpID
	}

	// Same grandmaster: use dscmp2 (IEEE 1588-2008, Figure 28).
	sA, sB := uint32(a.StepsRemoved), uint32(b.StepsRemoved)
	if sA+1 < sB {
		return -1
	}
	if sB+1 < sA {
		return 1
	}
	if sA < sB {
		return -1
	}
	if sA > sB {
		return 1
	}

	// Equal stepsRemoved: compare senders.
	cmpSender := bytes.Compare(a.Sender.ClockIdentity[:], b.Sender.ClockIdentity[:])
	if cmpSender != 0 {
		return cmpSender
	}
	if a.Sender.PortNumber != b.Sender.PortNumber {
		return int(a.Sender.PortNumber) - int(b.Sender.PortNumber)
	}
	// receiver port
	return int(a.Receiver.PortNumber) - int(b.Receiver.PortNumber)
}

// SelectBestMaster selects the best foreign master from the list.
// It returns nil if the list is empty.
func SelectBestMaster(masters []*ForeignMaster, receiverPort ptp.PortIdentity) *ForeignMaster {
	if len(masters) == 0 {
		return nil
	}
	best := masters[0]
	bestDS := DatasetFromAnnounce(best.Header, best.Announce, receiverPort)
	for _, m := range masters[1:] {
		ds := DatasetFromAnnounce(m.Header, m.Announce, receiverPort)
		if DSCmp(&ds, &bestDS) < 0 {
			best = m
			bestDS = ds
		}
	}
	return best
}
