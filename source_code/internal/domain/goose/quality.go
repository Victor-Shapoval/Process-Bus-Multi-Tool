package goose

// QualityBitLength is the IEC 61850 Quality BIT STRING size: validity (2),
// detailQuality (8), source, test, operatorBlocked and derived.
const QualityBitLength = 14

// NewQualityBitString converts a Quality mask whose bit numbers follow
// IEC 61850-7-3 into the MSB-first ASN.1 BIT STRING payload representation.
// The BER unused-bits octet is added by the encoder, not stored in DataValue.
func NewQualityBitString(quality uint32) DataValue {
	payload := make([]byte, (QualityBitLength+7)/8)
	for bit := 0; bit < QualityBitLength; bit++ {
		if quality&(1<<bit) != 0 {
			payload[bit/8] |= byte(0x80 >> (bit % 8))
		}
	}
	return DataValue{
		Type:      DataTypeBitString,
		Bytes:     payload,
		BitLength: QualityBitLength,
	}
}
