package depsuppress

import "sort"

type NodeRef struct {
	Kind string // "host" | "monitor"
	ID   int64
	Name string
}

type HostLite struct {
	ID          int64
	Name        string
	Environment string
	Role        string
}

// Не project-scoped: не знает про project_id и трогает каждый host в hosts —
// вызывающий обязан передавать edges/hosts/monitors ровно одного проекта.
func PreviewSuppression(edges []Edge, hosts []HostLite, monitors []NodeRef) map[NodeRef][]NodeRef {
	hostByID := make(map[int64]HostLite, len(hosts))
	for _, h := range hosts {
		hostByID[h.ID] = h
	}
	monitorByID := make(map[int64]NodeRef, len(monitors))
	for _, m := range monitors {
		monitorByID[m.ID] = m
	}

	out := map[NodeRef][]NodeRef{}
	seen := map[NodeRef]map[NodeRef]bool{}

	for _, e := range edges {
		parent, ok := previewResolveParent(e, hostByID, monitorByID)
		if !ok {
			continue
		}
		children := previewResolveChildren(e, hostByID, monitorByID, parent)
		if len(children) == 0 {
			continue
		}
		if seen[parent] == nil {
			seen[parent] = map[NodeRef]bool{}
		}
		for _, c := range children {
			if seen[parent][c] {
				continue
			}
			seen[parent][c] = true
			out[parent] = append(out[parent], c)
		}
	}

	for parent, children := range out {
		sort.Slice(children, func(i, j int) bool {
			if children[i].Kind != children[j].Kind {
				return children[i].Kind < children[j].Kind
			}
			return children[i].ID < children[j].ID
		})
		out[parent] = children
	}
	return out
}

func previewResolveParent(e Edge, hostByID map[int64]HostLite, monitorByID map[int64]NodeRef) (NodeRef, bool) {
	if e.ParentHostID != nil {
		h, ok := hostByID[*e.ParentHostID]
		if !ok {
			return NodeRef{}, false
		}
		return NodeRef{Kind: "host", ID: h.ID, Name: h.Name}, true
	}
	if e.ParentMonitorID != nil {
		m, ok := monitorByID[*e.ParentMonitorID]
		return m, ok
	}
	return NodeRef{}, false
}

func previewResolveChildren(e Edge, hostByID map[int64]HostLite, monitorByID map[int64]NodeRef, parent NodeRef) []NodeRef {
	switch {
	case e.ChildHostID != nil:
		h, ok := hostByID[*e.ChildHostID]
		if !ok {
			return nil
		}
		return []NodeRef{{Kind: "host", ID: h.ID, Name: h.Name}}
	case e.ChildMonitorID != nil:
		m, ok := monitorByID[*e.ChildMonitorID]
		if !ok {
			return nil
		}
		return []NodeRef{m}
	case e.ChildLabelScope != nil && e.ChildLabelValue != nil:
		return previewExpandLabel(*e.ChildLabelScope, *e.ChildLabelValue, hostByID, parent)
	default:
		return nil
	}
}

// Исключает самого parent — селектор, случайно совпавший с его env/role,
// не должен подавлять его самого.
func previewExpandLabel(scope, value string, hostByID map[int64]HostLite, parent NodeRef) []NodeRef {
	var out []NodeRef
	for _, h := range hostByID {
		if parent.Kind == "host" && h.ID == parent.ID {
			continue
		}
		var hv string
		switch scope {
		case "env":
			hv = h.Environment
		case "role":
			hv = h.Role
		default:
			continue
		}
		if hv == value {
			out = append(out, NodeRef{Kind: "host", ID: h.ID, Name: h.Name})
		}
	}
	return out
}
