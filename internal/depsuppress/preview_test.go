package depsuppress

import "testing"

func int64p(v int64) *int64 { return &v }

func strp(v string) *string { return &v }

// TestPreviewSuppressionLabelExcludesSelfMatch — parent gw (host, role=web)
// with a label-selector child (role=web) must expand to web1 (same role)
// but never to gw itself (self-match, MAJOR-5) nor to db1 (different role).
func TestPreviewSuppressionLabelExcludesSelfMatch(t *testing.T) {
	gw := HostLite{ID: 1, Name: "gw", Environment: "prod", Role: "web"}
	web1 := HostLite{ID: 2, Name: "web1", Environment: "prod", Role: "web"}
	db1 := HostLite{ID: 3, Name: "db1", Environment: "prod", Role: "db"}
	hosts := []HostLite{gw, web1, db1}

	edges := []Edge{
		{ID: 1, ParentHostID: int64p(gw.ID), ChildLabelScope: strp("role"), ChildLabelValue: strp("web")},
	}

	got := PreviewSuppression(edges, hosts, nil)

	parent := NodeRef{Kind: "host", ID: gw.ID, Name: gw.Name}
	children, ok := got[parent]
	if !ok {
		t.Fatalf("expected preview entry for parent %v, got none (map: %v)", parent, got)
	}
	want := []NodeRef{{Kind: "host", ID: web1.ID, Name: web1.Name}}
	if len(children) != len(want) || children[0] != want[0] {
		t.Fatalf("children = %v, want %v (gw itself and db1 must be excluded)", children, want)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one parent in preview, got %d: %v", len(got), got)
	}
}

// TestPreviewSuppressionExplicitHostChild — an explicit host-to-host edge is
// resolved as-is, without any label expansion.
func TestPreviewSuppressionExplicitHostChild(t *testing.T) {
	parentHost := HostLite{ID: 10, Name: "lb1", Environment: "prod", Role: "lb"}
	childHost := HostLite{ID: 11, Name: "app1", Environment: "prod", Role: "app"}
	hosts := []HostLite{parentHost, childHost}

	edges := []Edge{
		{ID: 2, ParentHostID: int64p(parentHost.ID), ChildHostID: int64p(childHost.ID)},
	}

	got := PreviewSuppression(edges, hosts, nil)

	parent := NodeRef{Kind: "host", ID: parentHost.ID, Name: parentHost.Name}
	want := NodeRef{Kind: "host", ID: childHost.ID, Name: childHost.Name}
	children, ok := got[parent]
	if !ok || len(children) != 1 || children[0] != want {
		t.Fatalf("children = %v (ok=%v), want [%v]", children, ok, want)
	}
}

// TestPreviewSuppressionMonitorChild — a monitor child is resolved from the
// supplied monitor inventory, not from hosts.
func TestPreviewSuppressionMonitorChild(t *testing.T) {
	parentHost := HostLite{ID: 20, Name: "gw2", Environment: "prod", Role: "web"}
	monitor := NodeRef{Kind: "monitor", ID: 30, Name: "ping-google"}

	edges := []Edge{
		{ID: 3, ParentHostID: int64p(parentHost.ID), ChildMonitorID: int64p(monitor.ID)},
	}

	got := PreviewSuppression(edges, []HostLite{parentHost}, []NodeRef{monitor})

	parent := NodeRef{Kind: "host", ID: parentHost.ID, Name: parentHost.Name}
	children, ok := got[parent]
	if !ok || len(children) != 1 || children[0] != monitor {
		t.Fatalf("children = %v (ok=%v), want [%v]", children, ok, monitor)
	}
}

// TestPreviewSuppressionDanglingParent — an edge whose parent no longer
// resolves in the supplied inventory (deleted since the edge was created)
// contributes nothing to the preview: dangling parent host, dangling parent
// monitor, and a malformed edge with no parent at all are all dropped.
func TestPreviewSuppressionDanglingParent(t *testing.T) {
	childHost := HostLite{ID: 51, Name: "web9", Environment: "prod", Role: "web"}
	monitor := NodeRef{Kind: "monitor", ID: 60, Name: "ping-api"}

	edges := []Edge{
		// Parent host id 999 was deleted after the edge was created.
		{ID: 7, ParentHostID: int64p(999), ChildHostID: int64p(childHost.ID)},
		// Parent monitor id 888 was deleted after the edge was created.
		{ID: 8, ParentMonitorID: int64p(888), ChildHostID: int64p(childHost.ID)},
		// Malformed edge without any parent (must never happen past
		// validateShape, but the preview must not panic or invent a parent).
		{ID: 9, ChildHostID: int64p(childHost.ID)},
	}

	got := PreviewSuppression(edges, []HostLite{childHost}, []NodeRef{monitor})

	if len(got) != 0 {
		t.Fatalf("expected empty preview for dangling/absent parents, got %v", got)
	}
}

// TestPreviewResolveParentBranches — unit coverage of previewResolveParent
// itself: each miss branch returns (NodeRef{}, false), a live monitor parent
// resolves to the inventory NodeRef.
func TestPreviewResolveParentBranches(t *testing.T) {
	hostByID := map[int64]HostLite{1: {ID: 1, Name: "gw", Environment: "prod", Role: "web"}}
	monitorByID := map[int64]NodeRef{2: {Kind: "monitor", ID: 2, Name: "ping"}}

	cases := []struct {
		name   string
		edge   Edge
		want   NodeRef
		wantOK bool
	}{
		{"host parent resolves", Edge{ParentHostID: int64p(1)}, NodeRef{Kind: "host", ID: 1, Name: "gw"}, true},
		{"host parent deleted", Edge{ParentHostID: int64p(999)}, NodeRef{}, false},
		{"monitor parent resolves", Edge{ParentMonitorID: int64p(2)}, NodeRef{Kind: "monitor", ID: 2, Name: "ping"}, true},
		{"monitor parent deleted", Edge{ParentMonitorID: int64p(888)}, NodeRef{}, false},
		{"no parent at all", Edge{}, NodeRef{}, false},
	}
	for _, tc := range cases {
		got, ok := previewResolveParent(tc.edge, hostByID, monitorByID)
		if ok != tc.wantOK || got != tc.want {
			t.Fatalf("%s: previewResolveParent = (%v, %v), want (%v, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestPreviewResolveChildrenBranches — unit coverage of the child-resolution
// misses: a deleted monitor child resolves to nothing, an edge with no child
// spec at all (must never happen past validateShape) resolves to nothing.
func TestPreviewResolveChildrenBranches(t *testing.T) {
	hostByID := map[int64]HostLite{}
	monitorByID := map[int64]NodeRef{}
	parent := NodeRef{Kind: "host", ID: 1, Name: "gw"}

	if got := previewResolveChildren(Edge{ChildMonitorID: int64p(777)}, hostByID, monitorByID, parent); got != nil {
		t.Fatalf("deleted monitor child: previewResolveChildren = %v, want nil", got)
	}
	if got := previewResolveChildren(Edge{}, hostByID, monitorByID, parent); got != nil {
		t.Fatalf("edge without child spec: previewResolveChildren = %v, want nil", got)
	}
}

// TestPreviewExpandLabelUnknownScope — a label scope outside {env,role}
// (must never happen past validateShape) expands to nothing instead of
// accidentally matching hosts.
func TestPreviewExpandLabelUnknownScope(t *testing.T) {
	hostByID := map[int64]HostLite{
		2: {ID: 2, Name: "web1", Environment: "prod", Role: "web"},
	}
	parent := NodeRef{Kind: "host", ID: 1, Name: "gw"}

	if got := previewExpandLabel("zone", "eu", hostByID, parent); got != nil {
		t.Fatalf("unknown scope: previewExpandLabel = %v, want nil", got)
	}
}

// TestPreviewSuppressionSortsMixedKinds — children of one parent are sorted
// by (kind, id): hosts before monitors, ids ascending within a kind.
func TestPreviewSuppressionSortsMixedKinds(t *testing.T) {
	parentHost := HostLite{ID: 70, Name: "gw4", Environment: "prod", Role: "web"}
	hostA := HostLite{ID: 72, Name: "web4", Environment: "prod", Role: "app"}
	hostB := HostLite{ID: 71, Name: "web3", Environment: "prod", Role: "app"}
	monitor := NodeRef{Kind: "monitor", ID: 5, Name: "ping-site"}

	edges := []Edge{
		{ID: 20, ParentHostID: int64p(parentHost.ID), ChildMonitorID: int64p(monitor.ID)},
		{ID: 21, ParentHostID: int64p(parentHost.ID), ChildHostID: int64p(hostA.ID)},
		{ID: 22, ParentHostID: int64p(parentHost.ID), ChildHostID: int64p(hostB.ID)},
	}

	got := PreviewSuppression(edges, []HostLite{parentHost, hostA, hostB}, []NodeRef{monitor})

	parent := NodeRef{Kind: "host", ID: parentHost.ID, Name: parentHost.Name}
	want := []NodeRef{
		{Kind: "host", ID: hostB.ID, Name: hostB.Name},
		{Kind: "host", ID: hostA.ID, Name: hostA.Name},
		monitor,
	}
	children := got[parent]
	if len(children) != len(want) {
		t.Fatalf("children = %v, want %v", children, want)
	}
	for i := range want {
		if children[i] != want[i] {
			t.Fatalf("children[%d] = %v, want %v (full: %v)", i, children[i], want[i], children)
		}
	}
}

// TestPreviewSuppressionDedupAndMissingNodes — two edges expanding to the
// same child collapse to one entry; a child id no longer present in the
// inventory (deleted node) is silently dropped rather than surfaced as a
// ghost entry.
func TestPreviewSuppressionDedupAndMissingNodes(t *testing.T) {
	parentHost := HostLite{ID: 40, Name: "gw3", Environment: "prod", Role: "web"}
	childHost := HostLite{ID: 41, Name: "web2", Environment: "prod", Role: "web"}
	hosts := []HostLite{parentHost, childHost}

	edges := []Edge{
		{ID: 4, ParentHostID: int64p(parentHost.ID), ChildHostID: int64p(childHost.ID)},
		{ID: 5, ParentHostID: int64p(parentHost.ID), ChildLabelScope: strp("role"), ChildLabelValue: strp("web")},
		// Dangling child id — node deleted since the edge was created.
		{ID: 6, ParentHostID: int64p(parentHost.ID), ChildHostID: int64p(999)},
	}

	got := PreviewSuppression(edges, hosts, nil)

	parent := NodeRef{Kind: "host", ID: parentHost.ID, Name: parentHost.Name}
	children, ok := got[parent]
	if !ok {
		t.Fatalf("expected preview entry for parent %v", parent)
	}
	want := []NodeRef{{Kind: "host", ID: childHost.ID, Name: childHost.Name}}
	if len(children) != len(want) || children[0] != want[0] {
		t.Fatalf("children = %v, want %v (explicit + label-expanded duplicate must collapse to one, dangling id dropped)", children, want)
	}
}
