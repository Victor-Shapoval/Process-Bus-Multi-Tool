package supervisor

import "pbmt/internal/domain/ethernet"

func buildVLAN(id *uint16, priority *uint8) *ethernet.VLANTag {
	if id == nil {
		return nil
	}
	pri := uint8(4)
	if priority != nil {
		pri = *priority
	}
	return &ethernet.VLANTag{Priority: pri, VID: *id}
}
