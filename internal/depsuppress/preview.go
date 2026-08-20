package depsuppress

import "sort"

// NodeRef identifies one node (host or monitor) in the dependency graph,
// already resolved to a human-readable name — the shape the dry-run preview
// hands to the template, so it never needs a second name lookup.
type NodeRef struct {
	Kind string // "host" | "monitor"
	ID   int64
	Name string
}

// HostLite is the minimal host projection PreviewSuppression needs to expand
// label-selector children (env/role match) without touching the database —
// a subset of host.Host, kept local to this package so preview.go stays a
// pure function with no dependency on internal/host.
type HostLite struct {
	ID          int64
	Name        string
	Environment string
	Role        string
}

// PreviewSuppression computes, for each parent node that has at least one
// dependency edge, which nodes would currently be suppressed if that
// parent's availability incident were open right now. Pure function — no DB,
// no time, safe to call on every render of the suppression screen.
//
// Explicit host/monitor children are resolved as-is. Label-selector children
// (ChildLabelScope/ChildLabelValue) are expanded to every host in the
// project whose env/role matches, EXCLUDING the parent host itself
// (self-match, MAJOR-5): a host must never appear as its own suppressed
// child, mirroring Store.checkSelfMatch which guards this at write time.
// This is belt-and-suspenders — the preview must hold even for edges that
// predate that guard (created before ErrSelfMatch existed) or reached the
// table through any other path.
//
// Children are deduplicated per parent (several edges/labels expanding to
// the same node collapse into one entry) and sorted by (kind, id) for a
// stable render order. A node id that no longer resolves in hosts/monitors
// (deleted since the edge was created) is silently dropped — the same
// "vanished node" handling as suppressionHostLabel/suppressionMonitorLabel
// use for the edge list itself, so the preview never names a ghost.
//
// NOT project-scoped: this function does not know about project_id at all —
// it trusts edges/hosts/monitors as given and expands every label selector
// against every host in the hosts slice. The caller MUST pass edges, hosts,
// and monitors belonging to EXACTLY ONE project (the same project whose
// screen renders the preview). Pass multi-project data and a label edge from
// project A will silently expand into project B's hosts — there is no
// project_id field on HostLite/NodeRef to catch this here. This differs from
// the runtime Suppressor.ParentDown path, which scopes its own queries by
// project_id at the SQL layer; PreviewSuppression has no such backstop
// because it is deliberately DB-free. The current call site
// (suppressionPreviewRows in internal/web/alert_suppression.go) is safe
// because it only ever loads the single project's hosts/monitors/edges — any
// future caller must preserve that invariant itself.
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

// previewResolveParent resolves an edge's parent to a NodeRef, or false if
// the parent no longer exists in the supplied inventory (deleted since the
// edge was created).
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

// previewResolveChildren resolves an edge's child(ren): a single NodeRef for
// an explicit host/monitor child, or the set of hosts matching a
// label-selector child (self-match excluded).
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

// previewExpandLabel returns every host matching scope/value, excluding the
// parent host itself (self-match, MAJOR-5) — a label edge whose selector
// happens to match the parent's own env/role must not suppress the parent.
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
