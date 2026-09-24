// Package mmsclient adapts libiec61850's read-only client services.
package mmsclient

/*
#cgo CFLAGS: -I${SRCDIR}/../../../deps/libiec61850/hal/inc -I${SRCDIR}/../../../deps/libiec61850/src/common/inc -I${SRCDIR}/../../../deps/libiec61850/src/mms/inc -I${SRCDIR}/../../../deps/libiec61850/src/iec61850/inc -I${SRCDIR}/../../../deps/libiec61850/src/logging
#include <stdlib.h>
#include "iec61850_client.h"
#include "mms_type_spec.h"

static int spec_size(MmsVariableSpecification* s) {
 switch (s->type) {
 case MMS_FLOAT: return s->typeSpec.floatingpoint.formatWidth;
 default: return MmsVariableSpecification_getSize(s);
 }
}
*/
import "C"

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unsafe"

	_ "pbmt/deps/libiec61850"
	"pbmt/internal/application/sclmodel"
)

// A Client belongs to one worker. Close must not race an in-flight native call.
// Native timeouts bound cancellation latency; context is checked between calls.
type Client struct {
	conn     C.IedConnection
	endpoint sclmodel.Endpoint
}

func Dial(ctx context.Context, endpoint sclmodel.Endpoint) (sclmodel.Client, error) {
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn := C.IedConnection_create()
	if conn == nil {
		return nil, fmt.Errorf("allocate MMS connection")
	}
	C.IedConnection_setConnectTimeout(conn, 3000)
	C.IedConnection_setRequestTimeout(conn, 2000)
	ip := C.CString(endpoint.IP)
	defer C.free(unsafe.Pointer(ip))
	var code C.IedClientError
	C.IedConnection_connect(conn, &code, ip, C.int(endpoint.Port))
	c := &Client{conn: conn, endpoint: endpoint}
	if code != C.IED_ERROR_OK {
		c.Close()
		return nil, fmt.Errorf("connect %s: %s", endpoint, C.GoString(C.IedClientError_toString(code)))
	}
	if err := ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		C.IedConnection_close(c.conn)
		C.IedConnection_destroy(c.conn)
		c.conn = nil
	}
}

func listStrings(list C.LinkedList) []string {
	if list == nil {
		return nil
	}
	defer C.LinkedList_destroy(list)
	var result []string
	for p := C.LinkedList_getNext(list); p != nil; p = C.LinkedList_getNext(p) {
		result = append(result, C.GoString((*C.char)(C.LinkedList_getData(p))))
	}
	return result
}

func copySpec(s *C.MmsVariableSpecification, depth int, budget *int) (*sclmodel.Type, error) {
	if s == nil || depth > 64 || *budget <= 0 {
		return nil, fmt.Errorf("invalid or excessively large MMS type structure")
	}
	*budget--
	n := &sclmodel.Type{Name: C.GoString(C.MmsVariableSpecification_getName(s)), Size: int(C.spec_size(s))}
	switch C.MmsVariableSpecification_getType(s) {
	case C.MMS_ARRAY:
		n.Kind = "ARRAY"
	case C.MMS_STRUCTURE:
		n.Kind = "STRUCTURE"
	case C.MMS_BOOLEAN:
		n.Kind = "BOOLEAN"
	case C.MMS_INTEGER:
		n.Kind = "INTEGER"
	case C.MMS_UNSIGNED:
		n.Kind = "UNSIGNED"
	case C.MMS_FLOAT:
		n.Kind = "FLOAT"
	case C.MMS_BIT_STRING:
		n.Kind = "BIT_STRING"
	case C.MMS_OCTET_STRING:
		n.Kind = "OCTET_STRING"
	case C.MMS_VISIBLE_STRING:
		n.Kind = "VISIBLE_STRING"
	case C.MMS_STRING:
		n.Kind = "MMS_STRING"
	case C.MMS_UTC_TIME:
		n.Kind = "UTC_TIME"
	case C.MMS_BINARY_TIME:
		n.Kind = "BINARY_TIME"
	default:
		n.Kind = fmt.Sprintf("MMS_TYPE_%d", C.MmsVariableSpecification_getType(s))
	}
	if n.Kind == "STRUCTURE" {
		for i := 0; i < n.Size; i++ {
			child, err := copySpec(C.MmsVariableSpecification_getChildSpecificationByIndex(s, C.int(i)), depth+1, budget)
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, child)
		}
	} else if n.Kind == "ARRAY" {
		child, err := copySpec(C.MmsVariableSpecification_getArrayElementSpecification(s), depth+1, budget)
		if err != nil {
			return nil, err
		}
		n.Children = []*sclmodel.Type{child}
	}
	return n, nil
}

func (c *Client) Discover(ctx context.Context, progress func(string)) (*sclmodel.Model, error) {
	model := &sclmodel.Model{Endpoint: c.endpoint, ReadAt: time.Now()}
	mms := C.IedConnection_getMmsConnection(c.conn)
	var code C.MmsError
	devices := listStrings(C.MmsConnection_getDomainNames(mms, &code))
	if code != C.MMS_ERROR_NONE {
		return nil, fmt.Errorf("read logical devices: MMS error %d", code)
	}
	sort.Strings(devices)
	budget := 500000
	for _, domain := range devices {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cd := C.CString(domain)
		names := listStrings(C.MmsConnection_getDomainVariableNames(mms, &code, cd))
		C.free(unsafe.Pointer(cd))
		if code != C.MMS_ERROR_NONE {
			return nil, fmt.Errorf("read %s directory: MMS error %d", domain, code)
		}
		seen := map[string]bool{}
		var lns []string
		for _, name := range names {
			ln := strings.SplitN(name, "$", 2)[0]
			if !seen[ln] {
				seen[ln] = true
				lns = append(lns, ln)
			}
		}
		sort.Strings(lns)
		ld := sclmodel.LogicalDevice{Name: domain}
		for _, ln := range lns {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if progress != nil {
				progress("Reading " + domain + "/" + ln)
			}
			cd, cl := C.CString(domain), C.CString(ln)
			spec := C.MmsConnection_getVariableAccessAttributes(mms, &code, cd, cl)
			C.free(unsafe.Pointer(cd))
			C.free(unsafe.Pointer(cl))
			if spec == nil || code != C.MMS_ERROR_NONE {
				if spec != nil {
					C.MmsVariableSpecification_destroy(spec)
				}
				return nil, fmt.Errorf("read %s/%s types: MMS error %d", domain, ln, code)
			}
			copied, err := copySpec(spec, 0, &budget)
			C.MmsVariableSpecification_destroy(spec)
			if err != nil {
				return nil, err
			}
			node := sclmodel.LogicalNode{Name: ln, Groups: copied.Children}
			ld.Nodes = append(ld.Nodes, node)
		}
		model.Devices = append(model.Devices, ld)
	}
	if len(model.Devices) == 0 {
		return nil, fmt.Errorf("device returned an empty model")
	}
	// DataSet membership preserves the server's order, including array references.
	for di := range model.Devices {
		ld := &model.Devices[di]
		cd := C.CString(ld.Name)
		var errCode C.IedClientError
		sets := listStrings(C.IedConnection_getLogicalDeviceDataSets(c.conn, &errCode, cd))
		C.free(unsafe.Pointer(cd))
		if errCode != C.IED_ERROR_OK {
			model.Warnings = append(model.Warnings, fmt.Sprintf("%s: DataSet directory unavailable (%d)", ld.Name, errCode))
			sets = nil // Control-block reads remain useful when directory access is denied.
		}
		for _, set := range sets {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			parts := strings.SplitN(set, "$", 2)
			if len(parts) != 2 {
				model.Warnings = append(model.Warnings, "Unrecognized DataSet: "+set)
				continue
			}
			ref := C.CString(ld.Name + "/" + parts[0] + "." + parts[1])
			var deletable C.bool
			members := listStrings(C.IedConnection_getDataSetDirectory(c.conn, &errCode, ref, &deletable))
			C.free(unsafe.Pointer(ref))
			if errCode != C.IED_ERROR_OK {
				model.Warnings = append(model.Warnings, "Cannot read DataSet "+set)
				continue
			}
			for ni := range ld.Nodes {
				if ld.Nodes[ni].Name == parts[0] {
					ld.Nodes[ni].DataSets = append(ld.Nodes[ni].DataSets, sclmodel.DataSet{Name: parts[1], Members: members})
				}
			}
		}
		for ni := range ld.Nodes {
			node := &ld.Nodes[ni]
			if err := c.readModelControls(ctx, ld.Name, node, &model.Warnings); err != nil {
				return nil, err
			}
			for _, group := range node.Groups {
				if group.Name != "GO" {
					continue
				}
				for _, cb := range group.Children {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					ref := C.CString(ld.Name + "/" + node.Name + "." + cb.Name)
					value := C.IedConnection_getGoCBValues(c.conn, &errCode, ref, nil)
					C.free(unsafe.Pointer(ref))
					if value == nil || errCode != C.IED_ERROR_OK {
						if value != nil {
							C.ClientGooseControlBlock_destroy(value)
						}
						model.Warnings = append(model.Warnings, "Cannot read GoCB "+cb.Name)
						continue
					}
					addr := C.ClientGooseControlBlock_getDstAddress(value)
					mac := fmt.Sprintf("%02X-%02X-%02X-%02X-%02X-%02X", addr.dstAddress[0], addr.dstAddress[1], addr.dstAddress[2], addr.dstAddress[3], addr.dstAddress[4], addr.dstAddress[5])
					node.GOOSE = append(node.GOOSE, sclmodel.GooseControl{Name: cb.Name, DataSet: C.GoString(C.ClientGooseControlBlock_getDatSet(value)), GoID: C.GoString(C.ClientGooseControlBlock_getGoID(value)), ConfRev: uint32(C.ClientGooseControlBlock_getConfRev(value)), MAC: mac, APPID: int(addr.appId), VLAN: int(addr.vlanId), Priority: int(addr.vlanPriority), MinTime: uint32(C.ClientGooseControlBlock_getMinTime(value)), MaxTime: uint32(C.ClientGooseControlBlock_getMaxTime(value))})
					C.ClientGooseControlBlock_destroy(value)
				}
			}
		}
	}
	return model, ctx.Err()
}

func (c *Client) readModelControls(ctx context.Context, domain string, node *sclmodel.LogicalNode, warnings *[]string) error {
	for _, group := range node.Groups {
		for _, spec := range group.Children {
			if err := ctx.Err(); err != nil {
				return err
			}
			if group.Name == "RP" || group.Name == "BR" {
				ref := domain + "/" + node.Name + "." + group.Name + "." + spec.Name
				cr := C.CString(ref)
				var code C.IedClientError
				value := C.IedConnection_getRCBValues(c.conn, &code, cr, nil)
				C.free(unsafe.Pointer(cr))
				if value == nil || code != C.IED_ERROR_OK {
					if value != nil {
						C.ClientReportControlBlock_destroy(value)
					}
					*warnings = append(*warnings, fmt.Sprintf("%s: cannot read report configuration (%d)", ref, code))
					continue
				}
				trg := C.ClientReportControlBlock_getTrgOps(value)
				opts := C.ClientReportControlBlock_getOptFlds(value)
				node.Reports = append(node.Reports, sclmodel.ReportControl{Name: spec.Name, Buffered: group.Name == "BR", ReportID: C.GoString(C.ClientReportControlBlock_getRptId(value)), DataSet: C.GoString(C.ClientReportControlBlock_getDataSetReference(value)), ConfRev: uint32(C.ClientReportControlBlock_getConfRev(value)), BufferTime: uint32(C.ClientReportControlBlock_getBufTm(value)), IntegrityPeriod: uint32(C.ClientReportControlBlock_getIntgPd(value)), Triggers: sclmodel.ReportTriggers{DataChange: trg&C.TRG_OPT_DATA_CHANGED != 0, QualityChange: trg&C.TRG_OPT_QUALITY_CHANGED != 0, DataUpdate: trg&C.TRG_OPT_DATA_UPDATE != 0, Integrity: trg&C.TRG_OPT_INTEGRITY != 0, GI: trg&C.TRG_OPT_GI != 0}, Options: sclmodel.ReportOptions{SeqNum: opts&C.RPT_OPT_SEQ_NUM != 0, TimeStamp: opts&C.RPT_OPT_TIME_STAMP != 0, ReasonCode: opts&C.RPT_OPT_REASON_FOR_INCLUSION != 0, DataSet: opts&C.RPT_OPT_DATA_SET != 0, DataRef: opts&C.RPT_OPT_DATA_REFERENCE != 0, BufOvfl: opts&C.RPT_OPT_BUFFER_OVERFLOW != 0, EntryID: opts&C.RPT_OPT_ENTRY_ID != 0, ConfRev: opts&C.RPT_OPT_CONF_REV != 0}})
				C.ClientReportControlBlock_destroy(value)
			}
			if node.Name == "LLN0" && group.Name == "SP" && spec.Name == "SGCB" {
				value, err := c.readSettingControl(domain, spec)
				if err != nil {
					*warnings = append(*warnings, err.Error())
				} else {
					node.Settings = value
				}
			}
		}
	}
	return ctx.Err()
}

func (c *Client) readSettingControl(domain string, spec *sclmodel.Type) (*sclmodel.SettingControl, error) {
	cd, ci := C.CString(domain), C.CString("LLN0$SP$SGCB")
	defer C.free(unsafe.Pointer(cd))
	defer C.free(unsafe.Pointer(ci))
	var code C.MmsError
	v := C.MmsConnection_readVariable(C.IedConnection_getMmsConnection(c.conn), &code, cd, ci)
	if v != nil {
		defer C.MmsValue_delete(v)
	}
	if code != C.MMS_ERROR_NONE || v == nil {
		return nil, fmt.Errorf("%s/LLN0.SGCB: read failed (%d)", domain, code)
	}
	if C.MmsValue_getType(v) != C.MMS_STRUCTURE || int(C.MmsValue_getArraySize(v)) != len(spec.Children) {
		return nil, fmt.Errorf("%s/LLN0.SGCB: structure changed", domain)
	}
	s := &sclmodel.SettingControl{}
	for i, field := range spec.Children {
		if field.Name != "NumOfSG" && field.Name != "ActSG" {
			continue
		}
		e := C.MmsValue_getElement(v, C.int(i))
		if e == nil || C.MmsValue_getType(e) != C.MMS_UNSIGNED {
			return nil, fmt.Errorf("%s/LLN0.SGCB: invalid %s", domain, field.Name)
		}
		n := uint32(C.MmsValue_toUint32(e))
		if field.Name == "NumOfSG" {
			s.NumGroups = n
		} else {
			s.ActiveGroup = n
		}
	}
	if s.NumGroups == 0 || s.ActiveGroup == 0 || s.ActiveGroup > s.NumGroups {
		return nil, fmt.Errorf("%s/LLN0.SGCB: invalid group numbers", domain)
	}
	return s, nil
}

func (c *Client) Read(ctx context.Context, targets []sclmodel.Target) (map[string]sclmodel.Reading, error) {
	result := make(map[string]sclmodel.Reading, len(targets))
	// Read each wire variable once, then select requested array/structure elements.
	groups := map[string][]sclmodel.Target{}
	var keys []string
	for _, t := range targets {
		key := t.Domain + "/" + t.Item
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], t)
	}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		group := groups[key]
		t := group[0]
		cd, ci := C.CString(t.Domain), C.CString(t.Item)
		var code C.MmsError
		v := C.MmsConnection_readVariable(C.IedConnection_getMmsConnection(c.conn), &code, cd, ci)
		C.free(unsafe.Pointer(cd))
		C.free(unsafe.Pointer(ci))
		now := time.Now()
		for _, target := range group {
			r := sclmodel.Reading{ReadAt: now}
			if v == nil || code != C.MMS_ERROR_NONE {
				r.Error = fmt.Sprintf("MMS error %d", code)
			} else {
				elem := v
				for _, idx := range target.Path {
					if elem == nil {
						break
					}
					kind := C.MmsValue_getType(elem)
					if (kind != C.MMS_STRUCTURE && kind != C.MMS_ARRAY) || idx < 0 || idx >= int(C.MmsValue_getArraySize(elem)) {
						elem = nil
						break
					}
					elem = C.MmsValue_getElement(elem, C.int(idx))
				}
				if elem == nil {
					r.Error = "Model changed: element is missing"
				} else if C.MmsValue_getType(elem) == C.MMS_DATA_ACCESS_ERROR {
					r.Error = fmt.Sprintf("Access error %d", C.MmsValue_getDataAccessError(elem))
				} else if C.MmsValue_getType(elem) == C.MMS_UTC_TIME {
					r.Value = time.UnixMilli(int64(C.MmsValue_getUtcTimeInMs(elem))).UTC().Format(time.RFC3339Nano)
				} else {
					var buffer [4096]C.char
					C.MmsValue_printToBuffer(elem, &buffer[0], C.int(len(buffer)))
					r.Value = C.GoString(&buffer[0])
				}
			}
			result[target.ID] = r
		}
		if v != nil {
			C.MmsValue_delete(v)
		}
		if C.IedConnection_getState(c.conn) != C.IED_STATE_CONNECTED {
			return result, fmt.Errorf("MMS connection lost")
		}
	}
	if C.IedConnection_getState(c.conn) != C.IED_STATE_CONNECTED {
		return result, fmt.Errorf("MMS connection lost")
	}
	return result, nil
}
