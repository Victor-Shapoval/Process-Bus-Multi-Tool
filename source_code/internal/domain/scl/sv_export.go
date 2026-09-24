package scl

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net"
	"strings"
)

const xsiNamespace = "http://www.w3.org/2001/XMLSchema-instance"

// SVPublisher contains the wire and control-block parameters required to
// describe PBMT's fixed eight-channel IEC 61850-9-2LE stream in SCL.
type SVPublisher struct {
	Name                  string
	DstMAC                string
	AppID                 uint16
	SvID                  string
	DatSet                string
	ConfRev               uint32
	VLANID                *uint16
	VLANPriority          *uint8
	SmpRate               uint16
	SampleTimingFrequency uint16
}

// MarshalSVPublisher builds a self-contained ICD model for one PBMT SV
// stream. Its FCDA order and scale factors match the actual 9-2LE payload:
// Ia, Ib, Ic, In at 1 mA/bit, then Ua, Ub, Uc, Un at 10 mV/bit, each followed
// by its quality word.
func MarshalSVPublisher(pub SVPublisher) ([]byte, error) {
	model, err := buildSVModel(pub)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.WriteString(xml.Header)
	enc := xml.NewEncoder(&out)
	enc.Indent("", "  ")
	if err := enc.Encode(model); err != nil {
		return nil, fmt.Errorf("encode SV SCL: %w", err)
	}
	if err := enc.Flush(); err != nil {
		return nil, fmt.Errorf("flush SV SCL: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func buildSVModel(pub SVPublisher) (sclDocument, error) {
	if strings.TrimSpace(pub.Name) == "" {
		return sclDocument{}, fmt.Errorf("publisher name is required")
	}
	mac, err := net.ParseMAC(pub.DstMAC)
	if err != nil || len(mac) != 6 {
		return sclDocument{}, fmt.Errorf("invalid SV destination MAC %q", pub.DstMAC)
	}
	if strings.TrimSpace(pub.SvID) == "" {
		return sclDocument{}, fmt.Errorf("sv_id is required")
	}
	if pub.SmpRate != 80 {
		return sclDocument{}, fmt.Errorf("9-2LE Publisher supports smp_rate 80")
	}
	if pub.SampleTimingFrequency != 50 && pub.SampleTimingFrequency != 60 {
		return sclDocument{}, fmt.Errorf("sample_timing_frequency must be 50 or 60 Hz")
	}
	if pub.VLANID != nil && *pub.VLANID > 4094 {
		return sclDocument{}, fmt.Errorf("vlan_id must be 0..4094")
	}
	if pub.VLANPriority != nil && *pub.VLANPriority > 7 {
		return sclDocument{}, fmt.Errorf("vlan_pri must be 0..7")
	}
	if pub.VLANPriority != nil && pub.VLANID == nil {
		return sclDocument{}, fmt.Errorf("vlan_id is required when vlan_pri is set")
	}

	datasetName, err := svDatasetName(pub.DatSet)
	if err != nil {
		return sclDocument{}, err
	}

	iedName := sanitizeName(pub.Name, "PBMT_SV")
	const (
		accessPoint = "P1"
		ldInst      = "MU01"
		controlName = "MSVCB01"
	)
	typeBase := iedName + ldInst
	ids := svTypeIDs{
		lln0:           typeBase + ".LLN0",
		lphd:           typeBase + ".LPHD1",
		tctr:           typeBase + ".TCTR1",
		tvtr:           typeBase + ".TVTR1",
		mod:            typeBase + ".LLN0.Mod",
		beh:            typeBase + ".LLN0.Beh",
		health:         typeBase + ".LLN0.Health",
		llnNameplate:   typeBase + ".LLN0.NamPlt",
		transNameplate: typeBase + ".TCTR1.NamPlt",
		measurement:    typeBase + ".TCTR1.Amp",
		physicalName:   typeBase + ".LPHD1.PhyNam",
		proxy:          typeBase + ".LPHD1.Proxy",
		instMag:        typeBase + ".TCTR1.Amp.instMag",
		scale:          typeBase + ".TCTR1.Amp.sVC",
		modEnum:        typeBase + ".Mod",
		behEnum:        typeBase + ".Beh",
		healthEnum:     typeBase + ".Health",
		ctlEnum:        typeBase + ".CtlModelKind",
	}

	address := address{Params: []addressParam{
		{Type: "MAC-Address", XSIType: "tP_MAC-Address", Value: formatMAC(mac)},
		{Type: "APPID", XSIType: "tP_APPID", Value: fmt.Sprintf("%04X", pub.AppID)},
	}}
	if pub.VLANID != nil {
		address.Params = append(address.Params, addressParam{Type: "VLAN-ID", XSIType: "tP_VLAN-ID", Value: fmt.Sprintf("%03X", *pub.VLANID)})
	}
	if pub.VLANPriority != nil {
		address.Params = append(address.Params, addressParam{Type: "VLAN-PRIORITY", XSIType: "tP_VLAN-PRIORITY", Value: fmt.Sprintf("%d", *pub.VLANPriority)})
	}

	return sclDocument{
		XMLNS:          namespace,
		XMLNSXSI:       xsiNamespace,
		SchemaLocation: namespace + " SCL.xsd",
		Header: header{
			ID:            "PBMT_" + iedName,
			Version:       "1",
			Revision:      fmt.Sprintf("%d", pub.ConfRev),
			ToolID:        "Process Bus Multi Tool",
			NameStructure: "IEDName",
		},
		Communication: communication{SubNetworks: []subNetwork{{
			Name: "ProcessBus",
			Type: "8-MMS",
			ConnectedAPs: []connectedAP{{
				IEDName: iedName,
				APName:  accessPoint,
				SMVs: []smv{{
					LDInst:  ldInst,
					CBName:  controlName,
					Address: address,
				}},
			}},
		}}},
		IEDs: []ied{{
			Name:         iedName,
			Manufacturer: "PBMT",
			Type:         "SV Publisher",
			Services:     services{GOOSE: serviceLimit{}},
			AccessPoints: []accessPointNode{{
				Name: accessPoint,
				Server: server{LDevices: []logicalDevice{{
					Inst: ldInst,
					LN0: logicalNodeZero{
						LNClass:  "LLN0",
						LNType:   ids.lln0,
						DataSets: []dataSet{{Name: datasetName, Desc: "9-2LE sampled values", FCDAs: svFCDAs(ldInst)}},
						SampledValueControls: []sampledValueControl{{
							Name:      controlName,
							SmvID:     strings.TrimSpace(pub.SvID),
							Multicast: true,
							DatSet:    datasetName,
							SmpRate:   pub.SmpRate,
							NofASDU:   1,
							ConfRev:   pub.ConfRev,
							SmvOpts:   smvOptions{},
						}},
						DOIs: []dataObjectInstance{svModeDOI(), svNameplateDOI()},
					},
					LNs: svLogicalNodes(ids),
				}}},
			}},
		}},
		DataTypes: svDataTypeTemplates(ids),
	}, nil
}

type svTypeIDs struct {
	lln0, lphd, tctr, tvtr                    string
	mod, beh, health, llnNameplate            string
	transNameplate, measurement, physicalName string
	proxy, instMag, scale                     string
	modEnum, behEnum, healthEnum, ctlEnum     string
}

func svDatasetName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "PhsMeas1", nil
	}
	if idx := strings.LastIndex(name, "$"); idx >= 0 {
		name = name[idx+1:]
	}
	if !validName(name) {
		return "", fmt.Errorf("dat_set %q does not contain a valid SCL dataset name", value)
	}
	return name, nil
}

func svFCDAs(ldInst string) []fcda {
	result := make([]fcda, 0, 16)
	for _, class := range []struct {
		lnClass string
		doName  string
	}{{"TCTR", "Amp"}, {"TVTR", "Vol"}} {
		for inst := 1; inst <= 4; inst++ {
			result = append(result,
				fcda{LDInst: ldInst, LNClass: class.lnClass, LNInst: fmt.Sprintf("%d", inst), DOName: class.doName, DAName: "instMag.i", FC: "MX"},
				fcda{LDInst: ldInst, LNClass: class.lnClass, LNInst: fmt.Sprintf("%d", inst), DOName: class.doName, DAName: "q", FC: "MX"},
			)
		}
	}
	return result
}

func svLogicalNodes(ids svTypeIDs) []logicalNode {
	result := []logicalNode{{Prefix: "", LNClass: "LPHD", Inst: "1", LNType: ids.lphd}}
	for _, class := range []struct {
		lnClass string
		lnType  string
		doName  string
		scale   string
	}{{"TCTR", ids.tctr, "Amp", "0.001"}, {"TVTR", ids.tvtr, "Vol", "0.01"}} {
		for inst := 1; inst <= 4; inst++ {
			result = append(result, logicalNode{
				Prefix:  "",
				LNClass: class.lnClass,
				Inst:    fmt.Sprintf("%d", inst),
				LNType:  class.lnType,
				DOIs: []dataObjectInstance{
					svModeDOI(),
					svMeasurementDOI(class.doName, class.scale),
				},
			})
		}
	}
	return result
}

func svModeDOI() dataObjectInstance {
	return dataObjectInstance{Name: "Mod", DAIs: []dataAttributeInstance{{Name: "ctlModel", Values: []string{"status-only"}}}}
}

func svNameplateDOI() dataObjectInstance {
	return dataObjectInstance{Name: "NamPlt", DAIs: []dataAttributeInstance{{Name: "ldNs", Values: []string{"IEC 61850-7-4:2003"}}}}
}

func svMeasurementDOI(name, scale string) dataObjectInstance {
	return dataObjectInstance{
		Name: name,
		DAIs: []dataAttributeInstance{{Name: "q"}},
		SDIs: []subDataInstance{
			{Name: "instMag", DAIs: []dataAttributeInstance{{Name: "i"}}},
			{Name: "sVC", DAIs: []dataAttributeInstance{
				{Name: "scaleFactor", Values: []string{scale}},
				{Name: "offset", Values: []string{"0.0"}},
			}},
		},
	}
}

func svDataTypeTemplates(ids svTypeIDs) dataTypeTemplates {
	commonDOs := []dataObjectRef{
		{Name: "Mod", Type: ids.mod},
		{Name: "Beh", Type: ids.beh},
		{Name: "Health", Type: ids.health},
		{Name: "NamPlt", Type: ids.llnNameplate},
	}
	transducerDOs := []dataObjectRef{
		{Name: "Mod", Type: ids.mod},
		{Name: "Beh", Type: ids.beh},
		{Name: "Health", Type: ids.health},
		{Name: "NamPlt", Type: ids.transNameplate},
	}
	tctrDOs := append(append([]dataObjectRef(nil), transducerDOs...), dataObjectRef{Name: "Amp", Type: ids.measurement})
	tvtrDOs := append(append([]dataObjectRef(nil), transducerDOs...), dataObjectRef{Name: "Vol", Type: ids.measurement})

	return dataTypeTemplates{
		LNodeTypes: []logicalNodeType{
			{ID: ids.lln0, LNClass: "LLN0", DOs: commonDOs},
			{ID: ids.lphd, LNClass: "LPHD", DOs: []dataObjectRef{
				{Name: "PhyNam", Type: ids.physicalName},
				{Name: "PhyHealth", Type: ids.health},
				{Name: "Proxy", Type: ids.proxy},
			}},
			{ID: ids.tctr, LNClass: "TCTR", DOs: tctrDOs},
			{ID: ids.tvtr, LNClass: "TVTR", DOs: tvtrDOs},
		},
		DOTypes: []dataObjectType{
			{ID: ids.physicalName, CDC: "DPL", DAs: []dataAttribute{{Name: "vendor", FC: "DC", BType: "VisString255"}}},
			{ID: ids.mod, CDC: "INC", DAs: []dataAttribute{
				{Name: "stVal", FC: "ST", BType: "Enum", Type: ids.modEnum},
				{Name: "q", FC: "ST", BType: "Quality"},
				{Name: "t", FC: "ST", BType: "Timestamp"},
				{Name: "ctlModel", FC: "CF", BType: "Enum", Type: ids.ctlEnum},
			}},
			{ID: ids.beh, CDC: "INS", DAs: []dataAttribute{
				{Name: "stVal", FC: "ST", BType: "Enum", Type: ids.behEnum},
				{Name: "q", FC: "ST", BType: "Quality"},
				{Name: "t", FC: "ST", BType: "Timestamp"},
			}},
			{ID: ids.health, CDC: "INS", DAs: []dataAttribute{
				{Name: "stVal", FC: "ST", BType: "Enum", Type: ids.healthEnum},
				{Name: "q", FC: "ST", BType: "Quality"},
				{Name: "t", FC: "ST", BType: "Timestamp"},
			}},
			{ID: ids.llnNameplate, CDC: "LPL", DAs: []dataAttribute{
				{Name: "vendor", FC: "DC", BType: "VisString255"},
				{Name: "swRev", FC: "DC", BType: "VisString255"},
				{Name: "d", FC: "DC", BType: "VisString255"},
				{Name: "configRev", FC: "DC", BType: "VisString255"},
				{Name: "ldNs", FC: "EX", BType: "VisString255"},
			}},
			{ID: ids.transNameplate, CDC: "LPL", DAs: []dataAttribute{
				{Name: "vendor", FC: "DC", BType: "VisString255"},
				{Name: "swRev", FC: "DC", BType: "VisString255"},
				{Name: "d", FC: "DC", BType: "VisString255"},
			}},
			{ID: ids.measurement, CDC: "SAV", DAs: []dataAttribute{
				{Name: "instMag", FC: "MX", BType: "Struct", Type: ids.instMag},
				{Name: "q", FC: "MX", BType: "Quality"},
				{Name: "sVC", FC: "CF", BType: "Struct", Type: ids.scale},
			}},
			{ID: ids.proxy, CDC: "SPS", DAs: []dataAttribute{
				{Name: "stVal", FC: "ST", BType: "BOOLEAN"},
				{Name: "q", FC: "ST", BType: "Quality"},
				{Name: "t", FC: "ST", BType: "Timestamp"},
			}},
		},
		DATypes: []dataAttributeType{
			{ID: ids.instMag, BDAs: []basicDataAttribute{{Name: "i", BType: "INT32"}}},
			{ID: ids.scale, BDAs: []basicDataAttribute{{Name: "scaleFactor", BType: "FLOAT32"}, {Name: "offset", BType: "FLOAT32"}}},
		},
		EnumTypes: []enumType{
			{ID: ids.behEnum, Values: []enumValue{{Ord: 1, Value: "on"}, {Ord: 2, Value: "blocked"}, {Ord: 3, Value: "test"}, {Ord: 4, Value: "test/blocked"}, {Ord: 5, Value: "off"}}},
			{ID: ids.modEnum, Values: []enumValue{{Ord: 1, Value: "on"}, {Ord: 2, Value: "blocked"}, {Ord: 3, Value: "test"}, {Ord: 4, Value: "test/blocked"}, {Ord: 5, Value: "off"}}},
			{ID: ids.healthEnum, Values: []enumValue{{Ord: 1, Value: "Ok"}, {Ord: 2, Value: "Warning"}, {Ord: 3, Value: "Alarm"}}},
			{ID: ids.ctlEnum, Values: []enumValue{{Ord: 0, Value: "status-only"}, {Ord: 1, Value: "direct-with-normal-security"}, {Ord: 2, Value: "sbo-with-normal-security"}, {Ord: 3, Value: "direct-with-enhanced-security"}, {Ord: 4, Value: "sbo-with-enhanced-security"}}},
		},
	}
}
