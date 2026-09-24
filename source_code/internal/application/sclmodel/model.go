// Package sclmodel describes an online IEC 61850 model without native pointers.
package sclmodel

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type Endpoint struct {
	IP   string
	Port int
}

func (e Endpoint) Validate() error {
	if net.ParseIP(e.IP) == nil {
		return fmt.Errorf("enter a valid device IP address")
	}
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}
func (e Endpoint) String() string { return net.JoinHostPort(e.IP, strconv.Itoa(e.Port)) }

// Type preserves the MMS structure order, widths and array element schema.
type Type struct {
	Name     string
	Kind     string
	Size     int
	Children []*Type
}
type LogicalNode struct {
	Name     string
	Groups   []*Type
	DataSets []DataSet
	GOOSE    []GooseControl
	Reports  []ReportControl `json:",omitempty"`
	Settings *SettingControl `json:",omitempty"`
}

// These are configuration values read with the model, not monitoring samples.
type SettingControl struct{ NumGroups, ActiveGroup uint32 }
type ReportControl struct {
	Name, ReportID, DataSet              string
	Buffered                             bool
	ConfRev, BufferTime, IntegrityPeriod uint32
	Triggers                             ReportTriggers
	Options                              ReportOptions
}
type ReportTriggers struct{ DataChange, QualityChange, DataUpdate, Integrity, GI bool }
type ReportOptions struct{ SeqNum, TimeStamp, ReasonCode, DataSet, DataRef, BufOvfl, EntryID, ConfRev bool }
type LogicalDevice struct {
	Name  string
	Nodes []LogicalNode
}
type DataSet struct {
	Name    string
	Members []string
}
type GooseControl struct {
	Name, DataSet, GoID, MAC string
	ConfRev                  uint32
	APPID, VLAN, Priority    int
	MinTime, MaxTime         uint32
}
type Model struct {
	Endpoint Endpoint
	ReadAt   time.Time
	Devices  []LogicalDevice
	Warnings []string
}

// Target reads a named MMS variable; Path selects children of an array/structure
// locally, avoiding invented array element object references on the wire.
type Target struct {
	ID, Domain, Item string
	Path             []int
}
type Reading struct {
	Value  string
	ReadAt time.Time
	Error  string
}

type Client interface {
	Discover(context.Context, func(string)) (*Model, error)
	Read(context.Context, []Target) (map[string]Reading, error)
	Close()
}
type Dialer func(context.Context, Endpoint) (Client, error)

func IsDataFC(fc string) bool {
	return strings.Contains(" ST MX CO SP SG SE SV CF DC EX SR BL OR ", " "+fc+" ")
}
