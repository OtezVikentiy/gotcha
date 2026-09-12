package depsuppress

import "testing"

func i64(v int64) *int64 { return &v }

func edgeHH(project, parent, child int64) Edge {
	return Edge{ProjectID: project, ParentHostID: i64(parent), ChildHostID: i64(child)}
}

func TestDownRootChainAndSelf(t *testing.T) {
	snap := &snapshot{
		edges:     []Edge{edgeHH(1, 1, 2), edgeHH(1, 2, 3)},
		downHosts: map[int64]bool{1: true, 2: true},
		hostLabels: map[int64]hostLabels{
			1: {projectID: 1}, 2: {projectID: 1}, 3: {projectID: 1},
		},
	}
	root, ok := downRootFromSnapshot(snap, node{kind: "host", id: 3})
	if !ok || root != (node{kind: "host", id: 1}) {
		t.Fatalf("C: want root host/1, got %v ok=%v", root, ok)
	}
	root, ok = downRootFromSnapshot(snap, node{kind: "host", id: 2})
	if !ok || root != (node{kind: "host", id: 1}) {
		t.Fatalf("B: want root host/1, got %v ok=%v", root, ok)
	}
	root, ok = downRootFromSnapshot(snap, node{kind: "host", id: 1})
	if !ok || root != (node{kind: "host", id: 1}) {
		t.Fatalf("A: want self root, got %v ok=%v", root, ok)
	}
	snap.downHosts = map[int64]bool{}
	if _, ok := downRootFromSnapshot(snap, node{kind: "host", id: 3}); ok {
		t.Fatalf("alive node must have no down root")
	}
}

func TestDownRootCycleWithoutRoot(t *testing.T) {
	snap := &snapshot{
		edges:     []Edge{edgeHH(1, 1, 2), edgeHH(1, 2, 1), edgeHH(1, 2, 3)},
		downHosts: map[int64]bool{1: true, 2: true},
		hostLabels: map[int64]hostLabels{
			1: {projectID: 1}, 2: {projectID: 1}, 3: {projectID: 1},
		},
	}
	if root, ok := downRootFromSnapshot(snap, node{kind: "host", id: 3}); ok {
		t.Fatalf("cycle without root must not anchor C, got %v", root)
	}
}

func TestDownRootDeterminism(t *testing.T) {
	snap := &snapshot{
		edges: []Edge{
			{ProjectID: 1, ParentMonitorID: i64(4), ChildHostID: i64(3)},
			{ProjectID: 1, ParentHostID: i64(5), ChildHostID: i64(3)},
			{ProjectID: 1, ParentHostID: i64(9), ChildHostID: i64(3)},
		},
		downHosts:    map[int64]bool{5: true, 9: true},
		downMonitors: map[int64]bool{4: true},
		hostLabels:   map[int64]hostLabels{3: {projectID: 1}, 5: {projectID: 1}, 9: {projectID: 1}},
	}
	root, ok := downRootFromSnapshot(snap, node{kind: "host", id: 3})
	if !ok || root != (node{kind: "host", id: 5}) {
		t.Fatalf("want host/5 (host before monitor, min id), got %v ok=%v", root, ok)
	}
}

func TestDeclaredChildrenFromSnapshot(t *testing.T) {
	scope, val := "role", "web"
	snap := &snapshot{
		edges: []Edge{
			edgeHH(1, 1, 2),
			{ProjectID: 1, ParentHostID: i64(1), ChildMonitorID: i64(7)},
			{ProjectID: 1, ParentHostID: i64(1), ChildLabelScope: &scope, ChildLabelValue: &val},
		},
		hostLabels: map[int64]hostLabels{
			1: {projectID: 1, role: "web"},
			2: {projectID: 1, role: "web"},
			3: {projectID: 1, role: "web"},
			8: {projectID: 2, role: "web"},
		},
	}
	got := declaredChildrenFromSnapshot(snap, node{kind: "host", id: 1})
	if got != 3 {
		t.Fatalf("want 3 declared children, got %d", got)
	}
}
