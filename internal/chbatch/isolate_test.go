package chbatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// alwaysPoison — предикат, признающий любую ошибку ядом (для тестов дробления).
func alwaysPoison(error) bool { return true }

func TestIsolatePoison_DropsOnlyBadRows(t *testing.T) {
	// Ряды 3 и 7 "ядовитые": их одиночная вставка падает серверной ошибкой.
	poison := map[int]bool{3: true, 7: true}
	var inserted []int
	insert := func(_ context.Context, rows []int) error {
		for _, r := range rows {
			if poison[r] {
				return errors.New("bad row " + string(rune('0'+r)))
			}
		}
		inserted = append(inserted, rows...)
		return nil
	}
	rows := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	dropped, unresolved := IsolatePoison(context.Background(), rows, insert, alwaysPoison)
	if dropped != 2 {
		t.Fatalf("want 2 dropped, got %d", dropped)
	}
	if len(unresolved) != 0 {
		t.Fatalf("want 0 unresolved, got %d (%v)", len(unresolved), unresolved)
	}
	// Все не-ядовитые ряды должны быть вставлены.
	if len(inserted) != 8 {
		t.Fatalf("want 8 inserted, got %d (%v)", len(inserted), inserted)
	}
	for _, r := range inserted {
		if poison[r] {
			t.Fatalf("poison row %d was inserted", r)
		}
	}
}

func TestIsolatePoison_AllGood(t *testing.T) {
	insert := func(_ context.Context, rows []int) error { return nil }
	d, u := IsolatePoison(context.Background(), []int{1, 2, 3}, insert, alwaysPoison)
	if d != 0 || len(u) != 0 {
		t.Fatalf("want 0 dropped / 0 unresolved, got %d / %d", d, len(u))
	}
}

func TestIsolatePoison_Empty(t *testing.T) {
	insert := func(_ context.Context, rows []int) error { return errors.New("must not be called") }
	d, u := IsolatePoison(context.Background(), nil, insert, alwaysPoison)
	if d != 0 || len(u) != 0 {
		t.Fatalf("want 0 dropped / 0 unresolved for empty, got %d / %d", d, len(u))
	}
}

// Транзиентный отказ: insert падает НЕ-ядовитой ошибкой на каждом ряду. Ничего
// не должно дропаться — все ряды возвращаются в unresolved в исходном порядке.
func TestIsolatePoison_TransientKeepsAllRows(t *testing.T) {
	insert := func(_ context.Context, rows []int) error { return errors.New("dial tcp: connection refused") }
	rows := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	dropped, unresolved := IsolatePoison(context.Background(), rows, insert, func(error) bool { return false })
	if dropped != 0 {
		t.Fatalf("want 0 dropped on transient, got %d", dropped)
	}
	if len(unresolved) != len(rows) {
		t.Fatalf("want %d unresolved, got %d", len(rows), len(unresolved))
	}
	for i, r := range unresolved {
		if r != rows[i] {
			t.Fatalf("unresolved order broken at %d: got %d want %d (%v)", i, r, rows[i], unresolved)
		}
	}
}

// Смешанный случай: часть рядов — серверный яд, часть — транзиент. Яд дропается,
// транзиент возвращается в unresolved.
func TestIsolatePoison_MixedPoisonAndTransient(t *testing.T) {
	serverBad := map[int]bool{2: true} // серверная ошибка (яд)
	transient := map[int]bool{5: true} // сетевая ошибка (не яд)
	insert := func(_ context.Context, rows []int) error {
		for _, r := range rows {
			if serverBad[r] {
				return &clickhouse.Exception{Code: 53, Message: "type mismatch"}
			}
			if transient[r] {
				return errors.New("connection reset by peer")
			}
		}
		return nil
	}
	isPoison := func(err error) bool { return IsServerDataError(err) }
	rows := []int{0, 1, 2, 3, 4, 5, 6, 7}
	dropped, unresolved := IsolatePoison(context.Background(), rows, insert, isPoison)
	if dropped != 1 {
		t.Fatalf("want 1 dropped (server poison), got %d", dropped)
	}
	if len(unresolved) != 1 || unresolved[0] != 5 {
		t.Fatalf("want unresolved=[5] (transient), got %v", unresolved)
	}
}

func TestIsServerDataError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ch exception type mismatch", &clickhouse.Exception{Code: 53, Message: "type mismatch"}, true},
		{"wrapped ch exception parse", fmt.Errorf("insert: %w", &clickhouse.Exception{Code: 27, Message: "cannot parse"}), true},
		{"ch data too large string", &clickhouse.Exception{Code: 131, Message: "too large string"}, true},
		// RA-1 (audit-3): транзиентные серверные исключения НЕ считаются ядом —
		// иначе перегрузка CH дропала бы валидные батчи.
		{"ch memory limit (transient)", &clickhouse.Exception{Code: 241, Message: "memory limit exceeded"}, false},
		{"ch timeout (transient)", &clickhouse.Exception{Code: 159, Message: "timeout exceeded"}, false},
		{"ch too many parts (transient)", &clickhouse.Exception{Code: 252, Message: "too many parts"}, false},
		{"ch no such column (schema, transient)", &clickhouse.Exception{Code: 16, Message: "no such column"}, false},
		{"ch unknown table (schema, transient)", &clickhouse.Exception{Code: 60, Message: "unknown table"}, false},
		{"plain network", errors.New("dial tcp: connection refused"), false},
		{"deadline", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"wrapped deadline", fmt.Errorf("send: %w", context.DeadlineExceeded), false},
	}
	for _, tc := range cases {
		if got := IsServerDataError(tc.err); got != tc.want {
			t.Errorf("%s: IsServerDataError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
