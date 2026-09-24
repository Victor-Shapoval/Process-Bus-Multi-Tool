package sclmodel

import (
	"fmt"
	"strconv"
)

type TreeNode struct {
	ID, Parent, Name, Type string
	Snapshot               string
	Children               []string
	Target                 *Target
}
type Tree struct{ Nodes map[string]*TreeNode }

func BuildTree(model *Model) (*Tree, error) {
	tree := &Tree{Nodes: map[string]*TreeNode{"": {}}}
	add := func(id, parent, name, kind string) *TreeNode {
		if n := tree.Nodes[id]; n != nil {
			return n
		}
		n := &TreeNode{ID: id, Parent: parent, Name: name, Type: kind}
		tree.Nodes[id] = n
		tree.Nodes[parent].Children = append(tree.Nodes[parent].Children, id)
		return n
	}
	var appendType func(*Type, string, string, string, string, []int, string) error
	appendType = func(t *Type, parent, domain, item, fc string, path []int, name string) error {
		if len(tree.Nodes) > 200000 {
			return fmt.Errorf("model exceeds the 200,000 display-node limit")
		}
		id := parent + "/" + name + "[" + fc + "]"
		kind := t.Kind
		if t.Size > 0 && kind != "STRUCTURE" {
			kind += fmt.Sprintf("(%d)", t.Size)
		}
		if fc != "" {
			kind += " [" + fc + "]"
		}
		n := add(id, parent, name, kind)
		if t.Kind == "ARRAY" {
			if len(t.Children) != 1 || t.Size < 0 || t.Size > 200000 {
				return fmt.Errorf("invalid array %s", item)
			}
			for i := 0; i < t.Size; i++ {
				p := append(append([]int{}, path...), i)
				if err := appendType(t.Children[0], id, domain, item, fc, p, "["+strconv.Itoa(i)+"]"); err != nil {
					return err
				}
			}
		} else if len(t.Children) > 0 {
			for i, ch := range t.Children {
				nextItem := item + "$" + ch.Name
				var nextPath []int
				if path != nil {
					nextItem = item
					nextPath = append(append([]int{}, path...), i)
				}
				if err := appendType(ch, id, domain, nextItem, fc, nextPath, ch.Name); err != nil {
					return err
				}
			}
		} else {
			n.Target = &Target{ID: id, Domain: domain, Item: item, Path: path}
		}
		return nil
	}
	for _, ld := range model.Devices {
		ldID := "ld/" + ld.Name
		add(ldID, "", ld.Name, "Logical Device")
		for _, ln := range ld.Nodes {
			lnID := ldID + "/" + ln.Name
			add(lnID, ldID, ln.Name, "Logical Node")
			for _, group := range ln.Groups {
				if IsDataFC(group.Name) {
					for _, obj := range group.Children {
						doID := lnID + "/do/" + obj.Name
						add(doID, lnID, obj.Name, "Data Object")
						if obj.Kind == "STRUCTURE" {
							for _, attr := range obj.Children {
								if err := appendType(attr, doID, ld.Name, ln.Name+"$"+group.Name+"$"+obj.Name+"$"+attr.Name, group.Name, nil, attr.Name); err != nil {
									return nil, err
								}
							}
						} else {
							if err := appendType(obj, doID, ld.Name, ln.Name+"$"+group.Name+"$"+obj.Name, group.Name, nil, obj.Name); err != nil {
								return nil, err
							}
						}
					}
				} else {
					groupID := lnID + "/fc/" + group.Name
					add(groupID, lnID, group.Name, "Control blocks")
					for _, obj := range group.Children {
						if err := appendType(obj, groupID, ld.Name, ln.Name+"$"+group.Name+"$"+obj.Name, group.Name, nil, obj.Name); err != nil {
							return nil, err
						}
					}
				}
			}
			if len(ln.DataSets) > 0 {
				dsRoot := lnID + "/datasets"
				add(dsRoot, lnID, "DataSets", "")
				for _, ds := range ln.DataSets {
					id := dsRoot + "/" + ds.Name
					add(id, dsRoot, ds.Name, "DataSet")
					for i, member := range ds.Members {
						add(fmt.Sprintf("%s/%d", id, i), id, fmt.Sprintf("%d. %s", i+1, member), "Member")
					}
				}
			}
			// SCL stores control configuration, not the full online MMS schema.
			// Show imported settings without inventing pollable control attributes.
			hasControl := func(fc, name string) bool {
				for _, g := range ln.Groups {
					if g.Name == fc {
						for _, obj := range g.Children {
							if obj.Name == name {
								return true
							}
						}
					}
				}
				return false
			}
			snapshot := func(name string, fields [][2]string) {
				root := lnID + "/saved-controls"
				add(root, lnID, "Saved control configuration", "Configuration snapshot")
				id := root + "/" + name
				add(id, root, name, "Configuration snapshot")
				for _, f := range fields {
					n := add(id+"/"+f[0], id, f[0], "Configuration snapshot")
					n.Snapshot = f[1]
				}
			}
			for _, r := range ln.Reports {
				fc := "RP"
				if r.Buffered {
					fc = "BR"
				}
				if !hasControl(fc, r.Name) {
					snapshot(fc+"."+r.Name, [][2]string{{"rptID", r.ReportID}, {"datSet", r.DataSet}, {"confRev", fmt.Sprint(r.ConfRev)}, {"bufTime", fmt.Sprint(r.BufferTime)}, {"intgPd", fmt.Sprint(r.IntegrityPeriod)}, {"TrgOps", fmt.Sprintf("%+v", r.Triggers)}, {"OptFields", fmt.Sprintf("%+v", r.Options)}})
				}
			}
			for _, g := range ln.GOOSE {
				if !hasControl("GO", g.Name) {
					snapshot("GO."+g.Name, [][2]string{{"goID", g.GoID}, {"datSet", g.DataSet}, {"confRev", fmt.Sprint(g.ConfRev)}, {"MAC", g.MAC}, {"APPID", fmt.Sprintf("0x%04X", g.APPID)}, {"VLAN-ID", fmt.Sprint(g.VLAN)}, {"VLAN-PRIORITY", fmt.Sprint(g.Priority)}, {"MinTime (ms)", fmt.Sprint(g.MinTime)}, {"MaxTime (ms)", fmt.Sprint(g.MaxTime)}})
				}
			}
			if sg := ln.Settings; sg != nil && !hasControl("SP", "SGCB") {
				snapshot("SP.SGCB", [][2]string{{"numOfSGs", fmt.Sprint(sg.NumGroups)}, {"actSG", fmt.Sprint(sg.ActiveGroup)}})
			}
		}
	}
	return tree, nil
}

func (t *Tree) Expanded(id string, open func(string) bool) bool {
	n := t.Nodes[id]
	if n == nil {
		return false
	}
	for n.Parent != "" {
		if !open(n.Parent) {
			return false
		}
		n = t.Nodes[n.Parent]
		if n == nil {
			return false
		}
	}
	return true
}
