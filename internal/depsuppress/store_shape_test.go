package depsuppress

// Табличный unit-тест validateShape живёт в package depsuppress (не
// depsuppress_test): функция неэкспортируемая и чистая (не ходит в БД),
// поэтому проверяется без testenv/контейнеров — тот же прецедент, что и
// suppressor_cache_test.go.

import (
	"errors"
	"testing"
)

// TestValidateShape перебирает все невалидные формы ребра (каждая обязана
// вернуть ErrInvalidEdge) и обе валидные комбинации родитель/ребёнок.
func TestValidateShape(t *testing.T) {
	cases := []struct {
		name    string
		edge    Edge
		wantErr bool
	}{
		{
			name:    "valid: host parent + host child",
			edge:    Edge{ParentHostID: int64p(1), ChildHostID: int64p(2)},
			wantErr: false,
		},
		{
			name:    "valid: monitor parent + label child",
			edge:    Edge{ParentMonitorID: int64p(1), ChildLabelScope: strp("env"), ChildLabelValue: strp("prod")},
			wantErr: false,
		},
		{
			name:    "zero parents",
			edge:    Edge{ChildHostID: int64p(2)},
			wantErr: true,
		},
		{
			name:    "two parents",
			edge:    Edge{ParentHostID: int64p(1), ParentMonitorID: int64p(3), ChildHostID: int64p(2)},
			wantErr: true,
		},
		{
			name:    "zero child specs",
			edge:    Edge{ParentHostID: int64p(1)},
			wantErr: true,
		},
		{
			name:    "two child specs (host + monitor)",
			edge:    Edge{ParentHostID: int64p(1), ChildHostID: int64p(2), ChildMonitorID: int64p(3)},
			wantErr: true,
		},
		{
			name:    "two child specs (host + label)",
			edge:    Edge{ParentHostID: int64p(1), ChildHostID: int64p(2), ChildLabelScope: strp("env"), ChildLabelValue: strp("prod")},
			wantErr: true,
		},
		{
			name:    "label scope without value",
			edge:    Edge{ParentHostID: int64p(1), ChildLabelScope: strp("env")},
			wantErr: true,
		},
		{
			name:    "label value without scope",
			edge:    Edge{ParentHostID: int64p(1), ChildLabelValue: strp("prod")},
			wantErr: true,
		},
		{
			name:    "label scope outside {env,role}",
			edge:    Edge{ParentHostID: int64p(1), ChildLabelScope: strp("zone"), ChildLabelValue: strp("eu")},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		err := validateShape(tc.edge)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: validateShape = nil, want ErrInvalidEdge", tc.name)
			}
			if !errors.Is(err, ErrInvalidEdge) {
				t.Fatalf("%s: validateShape = %v, want error wrapping ErrInvalidEdge", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: validateShape = %v, want nil", tc.name, err)
		}
	}
}

// TestCheckSelfLoop — host==host и monitor==monitor отвергаются, любые
// другие комбинации (включая разные id и разнотипные узлы) проходят.
func TestCheckSelfLoop(t *testing.T) {
	cases := []struct {
		name    string
		edge    Edge
		wantErr bool
	}{
		{"host self-loop", Edge{ParentHostID: int64p(1), ChildHostID: int64p(1)}, true},
		{"monitor self-loop", Edge{ParentMonitorID: int64p(2), ChildMonitorID: int64p(2)}, true},
		{"different hosts ok", Edge{ParentHostID: int64p(1), ChildHostID: int64p(2)}, false},
		{"different monitors ok", Edge{ParentMonitorID: int64p(1), ChildMonitorID: int64p(2)}, false},
		{"host parent, monitor child ok", Edge{ParentHostID: int64p(1), ChildMonitorID: int64p(1)}, false},
		{"label child ok", Edge{ParentHostID: int64p(1), ChildLabelScope: strp("env"), ChildLabelValue: strp("prod")}, false},
	}
	for _, tc := range cases {
		err := checkSelfLoop(tc.edge)
		if tc.wantErr {
			if !errors.Is(err, ErrSelfLoop) {
				t.Fatalf("%s: checkSelfLoop = %v, want ErrSelfLoop", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: checkSelfLoop = %v, want nil", tc.name, err)
		}
	}
}

// TestNodeFromIDs — приоритет host над monitor, nil при отсутствии обоих.
func TestNodeFromIDs(t *testing.T) {
	if got := nodeFromIDs(int64p(1), nil); got == nil || *got != (node{kind: "host", id: 1}) {
		t.Fatalf("nodeFromIDs(host) = %v, want {host 1}", got)
	}
	if got := nodeFromIDs(nil, int64p(2)); got == nil || *got != (node{kind: "monitor", id: 2}) {
		t.Fatalf("nodeFromIDs(monitor) = %v, want {monitor 2}", got)
	}
	if got := nodeFromIDs(nil, nil); got != nil {
		t.Fatalf("nodeFromIDs(nil,nil) = %v, want nil", got)
	}
}
