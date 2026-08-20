package depsuppress

// Табличные unit-тесты чистых функций матчинга рёбер по снимку
// (edgeMatchesChild / isSelfMatchParent) — без БД, тот же same-package
// прецедент, что и suppressor_cache_test.go: функции неэкспортируемые.

import "testing"

// TestEdgeMatchesChild перебирает ветки матчинга ребра на ребёнка (kind,
// nodeID) по снимку: явные host/monitor-дети, label-селекторы с изоляцией
// проекта и отсутствующими метками, неизвестный kind.
func TestEdgeMatchesChild(t *testing.T) {
	snap := &snapshot{
		hostLabels: map[int64]hostLabels{
			10: {projectID: 1, env: "prod", role: "web"},
			11: {projectID: 2, env: "prod", role: "web"},
		},
	}

	cases := []struct {
		name   string
		edge   Edge
		kind   string
		nodeID int64
		want   bool
	}{
		{
			name: "explicit host child matches",
			edge: Edge{ProjectID: 1, ChildHostID: int64p(10)},
			kind: "host", nodeID: 10, want: true,
		},
		{
			name: "explicit host child does not match другой хост",
			edge: Edge{ProjectID: 1, ChildHostID: int64p(10)},
			kind: "host", nodeID: 11, want: false,
		},
		{
			name: "label env matches",
			edge: Edge{ProjectID: 1, ChildLabelScope: strp("env"), ChildLabelValue: strp("prod")},
			kind: "host", nodeID: 10, want: true,
		},
		{
			name: "label role does not match",
			edge: Edge{ProjectID: 1, ChildLabelScope: strp("role"), ChildLabelValue: strp("db")},
			kind: "host", nodeID: 10, want: false,
		},
		{
			name: "label: хост отсутствует в hostLabels (удалён)",
			edge: Edge{ProjectID: 1, ChildLabelScope: strp("role"), ChildLabelValue: strp("web")},
			kind: "host", nodeID: 999, want: false,
		},
		{
			name: "label: project_id ребра не совпадает с проектом хоста",
			edge: Edge{ProjectID: 1, ChildLabelScope: strp("role"), ChildLabelValue: strp("web")},
			kind: "host", nodeID: 11, want: false,
		},
		{
			name: "host edge без child-спеки не матчит",
			edge: Edge{ProjectID: 1},
			kind: "host", nodeID: 10, want: false,
		},
		{
			name: "monitor child matches",
			edge: Edge{ProjectID: 1, ChildMonitorID: int64p(20)},
			kind: "monitor", nodeID: 20, want: true,
		},
		{
			name: "monitor child does not match другой монитор",
			edge: Edge{ProjectID: 1, ChildMonitorID: int64p(20)},
			kind: "monitor", nodeID: 21, want: false,
		},
		{
			name: "monitor kind без ChildMonitorID не матчит",
			edge: Edge{ProjectID: 1, ChildHostID: int64p(10)},
			kind: "monitor", nodeID: 10, want: false,
		},
		{
			name: "kind вне {host,monitor} → false",
			edge: Edge{ProjectID: 1, ChildHostID: int64p(10)},
			kind: "service", nodeID: 10, want: false,
		},
	}

	for _, tc := range cases {
		if got := edgeMatchesChild(tc.edge, snap, tc.kind, tc.nodeID); got != tc.want {
			t.Fatalf("%s: edgeMatchesChild = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsSelfMatchParent перебирает ветки исключения self-match: узел не
// должен считаться ребёнком ребра, чей родитель — он сам.
func TestIsSelfMatchParent(t *testing.T) {
	cases := []struct {
		name   string
		edge   Edge
		kind   string
		nodeID int64
		want   bool
	}{
		{
			name: "host parent совпадает с узлом",
			edge: Edge{ParentHostID: int64p(10)},
			kind: "host", nodeID: 10, want: true,
		},
		{
			name: "host parent другой узел",
			edge: Edge{ParentHostID: int64p(10)},
			kind: "host", nodeID: 11, want: false,
		},
		{
			name: "host kind при monitor-родителе — не self-match",
			edge: Edge{ParentMonitorID: int64p(10)},
			kind: "host", nodeID: 10, want: false,
		},
		{
			name: "monitor parent совпадает с узлом",
			edge: Edge{ParentMonitorID: int64p(20)},
			kind: "monitor", nodeID: 20, want: true,
		},
		{
			name: "monitor parent другой узел",
			edge: Edge{ParentMonitorID: int64p(20)},
			kind: "monitor", nodeID: 21, want: false,
		},
		{
			name: "kind вне {host,monitor} → false",
			edge: Edge{ParentHostID: int64p(10)},
			kind: "service", nodeID: 10, want: false,
		},
	}

	for _, tc := range cases {
		if got := isSelfMatchParent(tc.edge, tc.kind, tc.nodeID); got != tc.want {
			t.Fatalf("%s: isSelfMatchParent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParentIsDown — состояние «родитель упал» читается из соответствующей
// карты снимка (downHosts для host-родителя, downMonitors для monitor-
// родителя); ребро без родителя всегда false.
func TestParentIsDown(t *testing.T) {
	snap := &snapshot{
		downHosts:    map[int64]bool{10: true},
		downMonitors: map[int64]bool{20: true},
	}

	cases := []struct {
		name string
		edge Edge
		want bool
	}{
		{"host parent down", Edge{ParentHostID: int64p(10)}, true},
		{"host parent up", Edge{ParentHostID: int64p(11)}, false},
		{"monitor parent down", Edge{ParentMonitorID: int64p(20)}, true},
		{"monitor parent up", Edge{ParentMonitorID: int64p(21)}, false},
		{"no parent at all", Edge{}, false},
	}
	for _, tc := range cases {
		if got := parentIsDown(tc.edge, snap); got != tc.want {
			t.Fatalf("%s: parentIsDown = %v, want %v", tc.name, got, tc.want)
		}
	}
}
