// Package scl builds IEC 61850 Substation Configuration Language models.
package scl

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net"
	"strings"
)

const namespace = "http://www.iec.ch/61850/2003/SCL"

// GoosePublisher contains the parts of a GOOSE publisher required by an SCL
// model. Dataset order is significant and matches the GOOSE allData order.
type GoosePublisher struct {
	Name         string
	DstMAC       string
	AppID        uint16
	GocbRef      string
	DatSet       string
	GoID         string
	ConfRev      uint32
	VLANID       *uint16
	VLANPriority *uint8
	MinTimeMS    uint32
	MaxTimeMS    uint32
	Dataset      []DatasetEntry
}

// DatasetEntry describes one FCDA entry and its GOOSE allData type.
type DatasetEntry struct {
	Name string
	Type string
}

// MarshalGoosePublisher builds a self-contained Edition 2 SCL model that can
// be saved as CID, ICD, or SCD. The model contains one IED, one GOOSE control
// block, and one GGIO logical node holding the publisher dataset objects.
func MarshalGoosePublisher(pub GoosePublisher) ([]byte, error) {
	model, err := buildModel(pub)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.WriteString(xml.Header)
	enc := xml.NewEncoder(&out)
	enc.Indent("", "  ")
	if err := enc.Encode(model); err != nil {
		return nil, fmt.Errorf("encode SCL: %w", err)
	}
	if err := enc.Flush(); err != nil {
		return nil, fmt.Errorf("flush SCL: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func buildModel(pub GoosePublisher) (sclDocument, error) {
	if strings.TrimSpace(pub.Name) == "" {
		return sclDocument{}, fmt.Errorf("publisher name is required")
	}
	mac, err := net.ParseMAC(pub.DstMAC)
	if err != nil || len(mac) != 6 {
		return sclDocument{}, fmt.Errorf("invalid GOOSE destination MAC %q", pub.DstMAC)
	}
	if len(pub.Dataset) == 0 {
		return sclDocument{}, fmt.Errorf("dataset is empty")
	}

	dataRef, err := parseDataSetRef(pub.DatSet)
	if err != nil {
		return sclDocument{}, err
	}
	controlRef, err := parseGocbRef(pub.GocbRef)
	if err != nil {
		return sclDocument{}, err
	}
	if dataRef.ldName != controlRef.ldName {
		return sclDocument{}, fmt.Errorf("dat_set logical device %q differs from gocb_ref logical device %q", dataRef.ldName, controlRef.ldName)
	}

	groups, fcdas, err := buildDataset(pub.Dataset)
	if err != nil {
		return sclDocument{}, err
	}

	iedName := sanitizeName(pub.Name, "PBMT")
	const (
		accessPoint = "P1"
		ldInst      = "LD0"
		ln0TypeID   = "PBMT_LLN0"
		lphdTypeID  = "PBMT_LPHD"
		ggioTypeID  = "PBMT_GGIO"
	)

	address := address{
		Params: []addressParam{
			{Type: "MAC-Address", Value: formatMAC(mac)},
			{Type: "APPID", Value: fmt.Sprintf("%04X", pub.AppID)},
		},
	}
	if pub.VLANID != nil {
		address.Params = append(address.Params, addressParam{Type: "VLAN-ID", Value: fmt.Sprintf("%03X", *pub.VLANID)})
	}
	if pub.VLANPriority != nil {
		address.Params = append(address.Params, addressParam{Type: "VLAN-PRIORITY", Value: fmt.Sprintf("%d", *pub.VLANPriority)})
	}

	gseModel := gse{LDInst: ldInst, CBName: controlRef.control, Address: address}
	if pub.MinTimeMS > 0 {
		gseModel.MinTime = &timeValue{Unit: "s", Multiplier: "m", Value: pub.MinTimeMS}
	}
	if pub.MaxTimeMS > 0 {
		gseModel.MaxTime = &timeValue{Unit: "s", Multiplier: "m", Value: pub.MaxTimeMS}
	}

	goID := strings.TrimSpace(pub.GoID)
	if goID == "" {
		goID = controlRef.control
	}

	lnTypeDOs := commonLogicalNodeDOs()
	for _, group := range groups {
		lnTypeDOs = append(lnTypeDOs, dataObjectRef{Name: group.name, Desc: group.rawName, Type: group.typeID()})
	}

	return sclDocument{
		XMLNS:    namespace,
		Version:  "2007",
		Revision: "B",
		Release:  "4",
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
				GSEs:    []gse{gseModel},
			}},
		}}},
		IEDs: []ied{{
			Name:         iedName,
			Manufacturer: "PBMT",
			Type:         "GOOSE Publisher",
			Services:     services{GOOSE: serviceLimit{Max: 1}},
			AccessPoints: []accessPointNode{{
				Name: accessPoint,
				Server: server{LDevices: []logicalDevice{{
					Inst:   ldInst,
					LDName: dataRef.ldName,
					LN0: logicalNodeZero{
						LNClass:  "LLN0",
						LNType:   ln0TypeID,
						DataSets: []dataSet{{Name: dataRef.dataSet, Desc: "GOOSE dataset " + pub.Name, FCDAs: fcdas}},
						GSEControls: []gseControl{{
							Name:    controlRef.control,
							Type:    "GOOSE",
							AppID:   goID,
							DatSet:  dataRef.dataSet,
							ConfRev: pub.ConfRev,
						}},
					},
					LNs: []logicalNode{
						{Prefix: "", LNClass: "LPHD", Inst: "1", LNType: lphdTypeID},
						{Prefix: "", LNClass: "GGIO", Inst: "1", LNType: ggioTypeID},
					},
				}}},
			}},
		}},
		DataTypes: dataTypeTemplates{
			LNodeTypes: []logicalNodeType{
				{ID: ln0TypeID, LNClass: "LLN0", DOs: commonLogicalNodeDOs()},
				{ID: lphdTypeID, LNClass: "LPHD", DOs: []dataObjectRef{
					{Name: "PhyNam", Type: "PBMT_DPL"},
					{Name: "PhyHealth", Type: "PBMT_INS"},
					{Name: "Proxy", Type: "PBMT_SPS"},
				}},
				{ID: ggioTypeID, LNClass: "GGIO", DOs: lnTypeDOs},
			},
			DOTypes: buildDOTypes(groups),
			DATypes: buildDATypes(groups),
			EnumTypes: []enumType{{
				ID: "PBMT_CtlModels",
				Values: []enumValue{
					{Ord: 0, Value: "status-only"},
					{Ord: 1, Value: "direct-with-normal-security"},
					{Ord: 2, Value: "sbo-with-normal-security"},
					{Ord: 3, Value: "direct-with-enhanced-security"},
					{Ord: 4, Value: "sbo-with-enhanced-security"},
				},
			}},
		},
	}, nil
}

func commonLogicalNodeDOs() []dataObjectRef {
	return []dataObjectRef{
		{Name: "Mod", Type: "PBMT_INC_Mod"},
		{Name: "Beh", Type: "PBMT_INS"},
		{Name: "Health", Type: "PBMT_INS"},
		{Name: "NamPlt", Type: "PBMT_LPL"},
	}
}

type objectRef struct {
	ldName  string
	dataSet string
	control string
}

func parseDataSetRef(ref string) (objectRef, error) {
	left, name, ok := strings.Cut(strings.TrimSpace(ref), "$")
	if !ok || strings.Contains(name, "$") {
		return objectRef{}, fmt.Errorf("dat_set %q must have form LDName/LLN0$DataSet", ref)
	}
	ldName, lnName, ok := strings.Cut(left, "/")
	if !ok || strings.Contains(lnName, "/") || lnName != "LLN0" {
		return objectRef{}, fmt.Errorf("dat_set %q must reference LLN0", ref)
	}
	if !validName(ldName) || !validName(name) {
		return objectRef{}, fmt.Errorf("dat_set %q contains an invalid SCL name", ref)
	}
	return objectRef{ldName: ldName, dataSet: name}, nil
}

func parseGocbRef(ref string) (objectRef, error) {
	left, name, ok := strings.Cut(strings.TrimSpace(ref), "$GO$")
	if !ok || strings.Contains(name, "$") {
		return objectRef{}, fmt.Errorf("gocb_ref %q must have form LDName/LLN0$GO$ControlBlock", ref)
	}
	ldName, lnName, ok := strings.Cut(left, "/")
	if !ok || strings.Contains(lnName, "/") || lnName != "LLN0" {
		return objectRef{}, fmt.Errorf("gocb_ref %q must reference LLN0", ref)
	}
	if !validName(ldName) || !validName(name) {
		return objectRef{}, fmt.Errorf("gocb_ref %q contains an invalid SCL name", ref)
	}
	return objectRef{ldName: ldName, control: name}, nil
}

type dataGroup struct {
	rawName string
	name    string
	kind    string
}

func (g dataGroup) typeID() string {
	switch g.kind {
	case "int":
		return "PBMT_INS"
	case "uint":
		return "PBMT_INSU"
	case "float":
		return "PBMT_MV"
	case "string":
		return "PBMT_VSS"
	default:
		return "PBMT_SPS"
	}
}

func (g dataGroup) fc() string {
	if g.kind == "float" {
		return "MX"
	}
	return "ST"
}

func buildDataset(entries []DatasetEntry) ([]dataGroup, []fcda, error) {
	groups := make([]dataGroup, 0, len(entries))
	byRawName := make(map[string]int, len(entries))
	usedNames := map[string]bool{"Mod": true, "Beh": true, "Health": true, "NamPlt": true}

	for i, entry := range entries {
		rawName := datasetObjectName(entry)
		if rawName == "" {
			return nil, nil, fmt.Errorf("dataset item #%d name is required", i+1)
		}
		idx, exists := byRawName[rawName]
		if !exists {
			name := uniqueDataName(sanitizeDataName(rawName, fmt.Sprintf("Signal%d", i+1)), usedNames)
			idx = len(groups)
			byRawName[rawName] = idx
			groups = append(groups, dataGroup{rawName: rawName, name: name})
		}
		kind, err := valueKind(entry.Type)
		if err != nil {
			return nil, nil, fmt.Errorf("dataset item %q: %w", entry.Name, err)
		}
		if kind != "" {
			if groups[idx].kind != "" && groups[idx].kind != kind {
				return nil, nil, fmt.Errorf("dataset object %q mixes incompatible types %q and %q", rawName, groups[idx].kind, kind)
			}
			groups[idx].kind = kind
		}
	}

	fcdas := make([]fcda, 0, len(entries))
	seenRefs := make(map[string]bool, len(entries))
	for i, entry := range entries {
		group := groups[byRawName[datasetObjectName(entry)]]
		daName, err := dataAttributeName(entry.Type)
		if err != nil {
			return nil, nil, fmt.Errorf("dataset item %q: %w", entry.Name, err)
		}
		key := group.name + "." + daName
		if seenRefs[key] {
			return nil, nil, fmt.Errorf("dataset item #%d duplicates SCL reference %s", i+1, key)
		}
		seenRefs[key] = true
		fcdas = append(fcdas, fcda{
			LDInst:  "LD0",
			LNClass: "GGIO",
			LNInst:  "1",
			DOName:  group.name,
			DAName:  daName,
			FC:      group.fc(),
		})
	}
	return groups, fcdas, nil
}

func datasetObjectName(entry DatasetEntry) string {
	name := strings.TrimSpace(entry.Name)
	suffixes := []string{".mag.f", ".stVal", ".q", ".t"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func valueKind(valueType string) (string, error) {
	switch valueType {
	case "bool", "int", "uint", "float", "string":
		return valueType, nil
	case "quality", "utc_time":
		return "", nil
	default:
		return "", fmt.Errorf("unsupported type %q", valueType)
	}
}

func dataAttributeName(valueType string) (string, error) {
	switch valueType {
	case "bool", "int", "uint", "string":
		return "stVal", nil
	case "float":
		return "mag.f", nil
	case "quality":
		return "q", nil
	case "utc_time":
		return "t", nil
	default:
		return "", fmt.Errorf("unsupported type %q", valueType)
	}
}

func buildDOTypes(groups []dataGroup) []dataObjectType {
	used := map[string]bool{
		"PBMT_INC_Mod": true,
		"PBMT_INS":     true,
		"PBMT_LPL":     true,
		"PBMT_DPL":     true,
		"PBMT_SPS":     true,
	}
	result := []dataObjectType{
		{ID: "PBMT_INC_Mod", CDC: "INC", DAs: []dataAttribute{
			{Name: "q", BType: "Quality", FC: "ST", Qchg: true},
			{Name: "t", BType: "Timestamp", FC: "ST"},
			{Name: "ctlModel", BType: "Enum", Type: "PBMT_CtlModels", FC: "CF"},
		}},
		statusDOType("PBMT_INS", "INS", "INT32", "ST"),
		{ID: "PBMT_LPL", CDC: "LPL", DAs: []dataAttribute{
			{Name: "vendor", BType: "VisString255", FC: "DC"},
			{Name: "swRev", BType: "VisString255", FC: "DC"},
			{Name: "d", BType: "VisString255", FC: "DC"},
			{Name: "configRev", BType: "VisString255", FC: "DC"},
			{Name: "ldNs", BType: "VisString255", FC: "EX"},
		}},
		{ID: "PBMT_DPL", CDC: "DPL", DAs: []dataAttribute{
			{Name: "vendor", BType: "VisString255", FC: "DC"},
		}},
		statusDOType("PBMT_SPS", "SPS", "BOOLEAN", "ST"),
	}
	for _, group := range groups {
		id := group.typeID()
		if used[id] {
			continue
		}
		used[id] = true
		switch group.kind {
		case "int":
			result = append(result, statusDOType(id, "INS", "INT32", "ST"))
		case "uint":
			result = append(result, statusDOType(id, "INS", "INT32U", "ST"))
		case "float":
			result = append(result, dataObjectType{ID: id, CDC: "MV", DAs: []dataAttribute{
				{Name: "mag", BType: "Struct", Type: "PBMT_AnalogueValue", FC: "MX", Dchg: true},
				{Name: "q", BType: "Quality", FC: "MX", Qchg: true},
				{Name: "t", BType: "Timestamp", FC: "MX"},
			}})
		case "string":
			result = append(result, statusDOType(id, "VSS", "VisString255", "ST"))
		default:
			// PBMT_SPS is part of the common LPHD model and is already present.
		}
	}
	return result
}

func statusDOType(id, cdc, valueType, fc string) dataObjectType {
	return dataObjectType{ID: id, CDC: cdc, DAs: []dataAttribute{
		{Name: "stVal", BType: valueType, FC: fc, Dchg: true},
		{Name: "q", BType: "Quality", FC: fc, Qchg: true},
		{Name: "t", BType: "Timestamp", FC: fc},
	}}
}

func buildDATypes(groups []dataGroup) []dataAttributeType {
	for _, group := range groups {
		if group.kind == "float" {
			return []dataAttributeType{{ID: "PBMT_AnalogueValue", BDAs: []basicDataAttribute{{Name: "f", BType: "FLOAT32"}}}}
		}
	}
	return nil
}

func validName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 && !isASCIILetter(r) {
			return false
		}
		if !isASCIILetter(r) && !isASCIIDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

func sanitizeName(value, fallback string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if isASCIILetter(r) || isASCIIDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	result := strings.Trim(b.String(), "_")
	if result == "" {
		result = fallback
	}
	if !isASCIILetter(rune(result[0])) {
		result = "N_" + result
	}
	return result
}

func sanitizeDataName(value, fallback string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if isASCIILetter(r) || isASCIIDigit(r) {
			b.WriteRune(r)
		}
	}
	result := b.String()
	if result == "" {
		result = fallback
	}
	if !isASCIILetter(rune(result[0])) {
		result = "Signal" + result
	} else {
		first := result[0]
		if first >= 'a' && first <= 'z' {
			first -= 'a' - 'A'
			result = string(first) + result[1:]
		}
	}
	return result
}

func isASCIILetter(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
}

func isASCIIDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func uniqueDataName(base string, used map[string]bool) string {
	name := base
	for n := 2; used[name]; n++ {
		name = fmt.Sprintf("%s%d", base, n)
	}
	used[name] = true
	return name
}

func formatMAC(mac net.HardwareAddr) string {
	parts := make([]string, len(mac))
	for i, b := range mac {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, "-")
}

type sclDocument struct {
	XMLName        xml.Name          `xml:"SCL"`
	XMLNS          string            `xml:"xmlns,attr"`
	XMLNSXSI       string            `xml:"xmlns:xsi,attr,omitempty"`
	SchemaLocation string            `xml:"xsi:schemaLocation,attr,omitempty"`
	Version        string            `xml:"version,attr,omitempty"`
	Revision       string            `xml:"revision,attr,omitempty"`
	Release        string            `xml:"release,attr,omitempty"`
	Header         header            `xml:"Header"`
	Communication  communication     `xml:"Communication"`
	IEDs           []ied             `xml:"IED"`
	DataTypes      dataTypeTemplates `xml:"DataTypeTemplates"`
}

type header struct {
	ID            string `xml:"id,attr"`
	Version       string `xml:"version,attr,omitempty"`
	Revision      string `xml:"revision,attr,omitempty"`
	ToolID        string `xml:"toolID,attr,omitempty"`
	NameStructure string `xml:"nameStructure,attr,omitempty"`
}

type communication struct {
	SubNetworks []subNetwork `xml:"SubNetwork"`
}

type subNetwork struct {
	Name         string        `xml:"name,attr"`
	Type         string        `xml:"type,attr,omitempty"`
	ConnectedAPs []connectedAP `xml:"ConnectedAP"`
}

type connectedAP struct {
	IEDName string `xml:"iedName,attr"`
	APName  string `xml:"apName,attr"`
	GSEs    []gse  `xml:"GSE"`
	SMVs    []smv  `xml:"SMV"`
}

type smv struct {
	LDInst  string  `xml:"ldInst,attr"`
	CBName  string  `xml:"cbName,attr"`
	Address address `xml:"Address"`
}

type gse struct {
	LDInst  string     `xml:"ldInst,attr"`
	CBName  string     `xml:"cbName,attr"`
	Address address    `xml:"Address"`
	MinTime *timeValue `xml:"MinTime,omitempty"`
	MaxTime *timeValue `xml:"MaxTime,omitempty"`
}

type address struct {
	Params []addressParam `xml:"P"`
}

type addressParam struct {
	Type    string `xml:"type,attr"`
	XSIType string `xml:"xsi:type,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type timeValue struct {
	Unit       string `xml:"unit,attr,omitempty"`
	Multiplier string `xml:"multiplier,attr,omitempty"`
	Value      uint32 `xml:",chardata"`
}

type ied struct {
	Name         string            `xml:"name,attr"`
	Manufacturer string            `xml:"manufacturer,attr,omitempty"`
	Type         string            `xml:"type,attr,omitempty"`
	Services     services          `xml:"Services"`
	AccessPoints []accessPointNode `xml:"AccessPoint"`
}

type services struct {
	GOOSE serviceLimit `xml:"GOOSE"`
}

type serviceLimit struct {
	Max uint32 `xml:"max,attr"`
}

type accessPointNode struct {
	Name   string `xml:"name,attr"`
	Server server `xml:"Server"`
}

type server struct {
	LDevices []logicalDevice `xml:"LDevice"`
}

type logicalDevice struct {
	Inst   string          `xml:"inst,attr"`
	LDName string          `xml:"ldName,attr,omitempty"`
	LN0    logicalNodeZero `xml:"LN0"`
	LNs    []logicalNode   `xml:"LN"`
}

type logicalNodeZero struct {
	LNClass              string                `xml:"lnClass,attr"`
	Inst                 string                `xml:"inst,attr"`
	LNType               string                `xml:"lnType,attr"`
	DataSets             []dataSet             `xml:"DataSet"`
	GSEControls          []gseControl          `xml:"GSEControl"`
	SampledValueControls []sampledValueControl `xml:"SampledValueControl"`
	DOIs                 []dataObjectInstance  `xml:"DOI"`
}

type logicalNode struct {
	Prefix  string               `xml:"prefix,attr"`
	LNClass string               `xml:"lnClass,attr"`
	Inst    string               `xml:"inst,attr"`
	LNType  string               `xml:"lnType,attr"`
	DOIs    []dataObjectInstance `xml:"DOI"`
}

type sampledValueControl struct {
	Name      string     `xml:"name,attr"`
	SmvID     string     `xml:"smvID,attr"`
	Multicast bool       `xml:"multicast,attr"`
	DatSet    string     `xml:"datSet,attr"`
	SmpRate   uint16     `xml:"smpRate,attr"`
	NofASDU   uint16     `xml:"nofASDU,attr"`
	ConfRev   uint32     `xml:"confRev,attr"`
	SmvOpts   smvOptions `xml:"SmvOpts"`
}

type smvOptions struct {
	RefreshTime bool `xml:"refreshTime,attr,omitempty"`
	SampleRate  bool `xml:"sampleRate,attr,omitempty"`
	DataSet     bool `xml:"dataSet,attr,omitempty"`
	Security    bool `xml:"security,attr,omitempty"`
}

type dataObjectInstance struct {
	Name string                  `xml:"name,attr"`
	DAIs []dataAttributeInstance `xml:"DAI"`
	SDIs []subDataInstance       `xml:"SDI"`
}

type subDataInstance struct {
	Name string                  `xml:"name,attr"`
	DAIs []dataAttributeInstance `xml:"DAI"`
}

type dataAttributeInstance struct {
	Name   string   `xml:"name,attr"`
	Values []string `xml:"Val"`
}

type dataSet struct {
	Name  string `xml:"name,attr"`
	Desc  string `xml:"desc,attr,omitempty"`
	FCDAs []fcda `xml:"FCDA"`
}

type fcda struct {
	LDInst  string `xml:"ldInst,attr"`
	Prefix  string `xml:"prefix,attr,omitempty"`
	LNClass string `xml:"lnClass,attr"`
	LNInst  string `xml:"lnInst,attr,omitempty"`
	DOName  string `xml:"doName,attr"`
	DAName  string `xml:"daName,attr,omitempty"`
	FC      string `xml:"fc,attr"`
}

type gseControl struct {
	Name    string `xml:"name,attr"`
	Type    string `xml:"type,attr"`
	AppID   string `xml:"appID,attr"`
	DatSet  string `xml:"datSet,attr"`
	ConfRev uint32 `xml:"confRev,attr"`
}

type dataTypeTemplates struct {
	LNodeTypes []logicalNodeType   `xml:"LNodeType"`
	DOTypes    []dataObjectType    `xml:"DOType"`
	DATypes    []dataAttributeType `xml:"DAType,omitempty"`
	EnumTypes  []enumType          `xml:"EnumType,omitempty"`
}

type logicalNodeType struct {
	ID      string          `xml:"id,attr"`
	LNClass string          `xml:"lnClass,attr"`
	DOs     []dataObjectRef `xml:"DO"`
}

type dataObjectRef struct {
	Name string `xml:"name,attr"`
	Desc string `xml:"desc,attr,omitempty"`
	Type string `xml:"type,attr"`
}

type dataObjectType struct {
	ID  string          `xml:"id,attr"`
	CDC string          `xml:"cdc,attr"`
	DAs []dataAttribute `xml:"DA"`
}

type dataAttribute struct {
	Name  string `xml:"name,attr"`
	BType string `xml:"bType,attr"`
	Type  string `xml:"type,attr,omitempty"`
	FC    string `xml:"fc,attr"`
	Dchg  bool   `xml:"dchg,attr,omitempty"`
	Qchg  bool   `xml:"qchg,attr,omitempty"`
}

type dataAttributeType struct {
	ID   string               `xml:"id,attr"`
	BDAs []basicDataAttribute `xml:"BDA"`
}

type basicDataAttribute struct {
	Name  string `xml:"name,attr"`
	BType string `xml:"bType,attr"`
}

type enumType struct {
	ID     string      `xml:"id,attr"`
	Values []enumValue `xml:"EnumVal"`
}

type enumValue struct {
	Ord   int    `xml:"ord,attr"`
	Value string `xml:",chardata"`
}
