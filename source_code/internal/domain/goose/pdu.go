// Package goose contains domain types and ports for IEC 61850-8-1 GOOSE.
package goose

import (
	"net"
	"time"

	"pbmt/internal/domain/ethernet"
)

// EtherType is the GOOSE EtherType defined by IEC 61850-8-1.
const EtherType = 0x88B8

// PDU is a decoded GOOSE message.
type PDU struct {
	// Ethernet header.
	DstMAC net.HardwareAddr
	SrcMAC net.HardwareAddr
	VLAN   *ethernet.VLANTag // nil = untagged

	// APDU header.
	AppID      uint16
	Simulation bool // S bit in Reserved1 (IEC 61850 Ed2.1 / 90-5)

	// goosePDU (IEC 61850-8-1, Section 8.2).
	GocbRef             string
	TimeAllowedToLiveMs uint32
	DatSet              string
	GoID                string
	Timestamp           time.Time   // t field, UtcTime
	TimeQuality         TimeQuality // eighth byte of UtcTime
	StNum               uint32
	SqNum               uint32
	Test                bool
	ConfRev             uint32
	NdsCom              bool
	NumDatSetEntries    uint32
	AllData             []DataValue
}

// TimeQuality is the quality of a UtcTime timestamp (IEC 61850-7-2).
// It is encoded in the eighth UtcTime byte: bits 7..5 are flags and bits 4..0 are accuracy.
type TimeQuality struct {
	LeapSecondsKnown     bool  // bit 7: true when leap seconds are known
	ClockFailure         bool  // bit 6: true when the source clock has failed
	ClockNotSynchronized bool  // bit 5: true when the source is not synchronized
	Accuracy             uint8 // bits 4..0: 0..24 = 2^-n-second accuracy; 31 = unspecified
}

// AccuracyUnspecified is the special Accuracy value defined by IEC 61850-7-2.
const AccuracyUnspecified uint8 = 31

// Valid reports whether the timestamp is usable (the clock is synchronized and
// has not failed).
func (q TimeQuality) Valid() bool {
	return !q.ClockFailure && !q.ClockNotSynchronized
}

// DataType identifies IEC 61850-8-1 Data CHOICE element types.
// Values correspond to context-specific ASN.1 BER tags (IEC 61850-8-1, §8.2.3.2).
type DataType uint8

const (
	DataTypeUnknown       DataType = 0
	DataTypeArray         DataType = 1  // [1] SEQUENCE OF Data
	DataTypeStructure     DataType = 2  // [2] SEQUENCE OF Data
	DataTypeBoolean       DataType = 3  // [3] BOOLEAN
	DataTypeBitString     DataType = 4  // [4] BIT STRING
	DataTypeInteger       DataType = 5  // [5] INTEGER (signed)
	DataTypeUnsigned      DataType = 6  // [6] INTEGER (unsigned)
	DataTypeFloatingPoint DataType = 7  // [7] FloatingPoint
	DataTypeReal          DataType = 8  // [8] REAL
	DataTypeOctetString   DataType = 9  // [9] OCTET STRING
	DataTypeVisibleString DataType = 10 // [10] VisibleString
	DataTypeBinaryTime    DataType = 12 // [12] TimeOfDay
	DataTypeBCD           DataType = 13 // [13] INTEGER (BCD)
	DataTypeBooleanArray  DataType = 14 // [14] BIT STRING (boolean array)
	DataTypeUTCTime       DataType = 17 // [17] UtcTime
)

// String returns a readable type name.
func (t DataType) String() string {
	switch t {
	case DataTypeArray:
		return "ARRAY"
	case DataTypeStructure:
		return "STRUCTURE"
	case DataTypeBoolean:
		return "BOOLEAN"
	case DataTypeBitString:
		return "BIT_STRING"
	case DataTypeInteger:
		return "INTEGER"
	case DataTypeUnsigned:
		return "UNSIGNED"
	case DataTypeFloatingPoint:
		return "FLOAT"
	case DataTypeReal:
		return "REAL"
	case DataTypeOctetString:
		return "OCTET_STRING"
	case DataTypeVisibleString:
		return "VISIBLE_STRING"
	case DataTypeBinaryTime:
		return "BINARY_TIME"
	case DataTypeBCD:
		return "BCD"
	case DataTypeBooleanArray:
		return "BOOLEAN_ARRAY"
	case DataTypeUTCTime:
		return "UTC_TIME"
	default:
		return "UNKNOWN"
	}
}

// DataValue is a tagged GOOSE allData value. Only the field selected by Type
// is meaningful. Children is meaningful for STRUCTURE and ARRAY values.
// This representation is convenient for serialization, including UI/MCP JSON.
type DataValue struct {
	Type        DataType
	Bool        bool
	Int         int64       // INTEGER, BCD
	UInt        uint64      // UNSIGNED
	Float       float64     // FLOAT, REAL
	Bytes       []byte      // payload octets; BIT STRING excludes the BER unused-bits octet
	BitLength   int         // significant bits for BIT STRING/BOOLEAN_ARRAY; 0 means len(Bytes)*8
	String      string      // VISIBLE_STRING
	Time        time.Time   // UTC_TIME, BINARY_TIME
	TimeQuality TimeQuality // UTC_TIME quality
	Children    []DataValue // STRUCTURE, ARRAY
}
