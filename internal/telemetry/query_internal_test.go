package telemetry

import (
	"strings"
	"testing"
)

// TestTxSubjectConds проверяет построение условий отбора субъекта в transactions
// без ClickHouse: важно, что conds и args идут строго параллельно (N-е условие ↔
// N-й параметр), а IP-only субъект не даёт условий (в transactions IP не хранят).
func TestTxSubjectConds(t *testing.T) {
	tests := []struct {
		name      string
		sub       Subject
		wantConds []string
		wantArgs  []any
	}{
		{
			name:      "empty",
			sub:       Subject{},
			wantConds: nil,
			wantArgs:  nil,
		},
		{
			name:      "ip only — transactions IP не хранят",
			sub:       Subject{IP: "10.0.0.1"},
			wantConds: nil,
			wantArgs:  nil,
		},
		{
			name: "user_id",
			sub:  Subject{UserID: "u1"},
			wantConds: []string{
				"user_id = ?", "tags['user.id'] = ?", "tags['enduser.id'] = ?",
			},
			wantArgs: []any{"u1", "u1", "u1"},
		},
		{
			name: "email",
			sub:  Subject{Email: "a@b.com"},
			wantConds: []string{
				"tags['user.email'] = ?", "tags['enduser.email'] = ?",
			},
			wantArgs: []any{"a@b.com", "a@b.com"},
		},
		{
			name: "user_id + email",
			sub:  Subject{UserID: "u1", Email: "a@b.com"},
			wantConds: []string{
				"user_id = ?", "tags['user.id'] = ?", "tags['enduser.id'] = ?",
				"tags['user.email'] = ?", "tags['enduser.email'] = ?",
			},
			wantArgs: []any{"u1", "u1", "u1", "a@b.com", "a@b.com"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conds, args := txSubjectConds(tc.sub)

			// Число условий и параметров обязано совпадать: каждый "?" в SQL
			// должен получить ровно один bound-параметр, иначе запрос сломан.
			if len(conds) != len(args) {
				t.Fatalf("len(conds)=%d != len(args)=%d", len(conds), len(args))
			}
			// Число плейсхолдеров "?" в условиях == числу параметров.
			ph := 0
			for _, c := range conds {
				ph += strings.Count(c, "?")
			}
			if ph != len(args) {
				t.Fatalf("плейсхолдеров %d != параметров %d", ph, len(args))
			}
			if !eqStr(conds, tc.wantConds) {
				t.Errorf("conds = %v, ждали %v", conds, tc.wantConds)
			}
			if !eqAny(args, tc.wantArgs) {
				t.Errorf("args = %v, ждали %v", args, tc.wantArgs)
			}
		})
	}
}

func eqStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func eqAny(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
