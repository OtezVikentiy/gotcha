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
