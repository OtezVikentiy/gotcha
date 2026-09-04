package profile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestFlameCapsStacksByWeight: стеков на входе больше maxFlameStacks →
// флеймграф собран ровно из maxFlameStacks САМЫХ ТЯЖЁЛЫХ, лёгкий хвост
// отрезан. Без LIMIT дерево росло бы с числом уникальных стеков без верхней
// границы; без ORDER BY усечение резало бы произвольные стеки, а не лёгкие.
//
// Проверка и Flame (окно/сервис), и FlameForTrace (trace_id) на одном посеве:
// оба запроса делят потолок и порядок усечения.
func TestFlameCapsStacksByWeight(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC()

	const extra = 5
	total := maxFlameStacks + extra
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO profile_samples (
		project_id, profile_type, service, environment, transaction, platform, ts, stack, value, unit, trace_id)`)
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}
	// Стек i весит i+1: самые лёгкие — f0..f4, они и должны отпасть.
	for i := 0; i < total; i++ {
		if err := batch.Append(uint64(31), "cpu", "api", "", "", "go", now.Add(-time.Minute),
			[]string{"root", fmt.Sprintf("f%d", i)}, uint64(i+1), "nanoseconds", "T"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Сумма весов extra+1..total — ровно то, что остаётся после отрезания
	// extra самых лёгких стеков (весов 1..extra).
	var wantValue uint64
	for v := extra + 1; v <= total; v++ {
		wantValue += uint64(v)
	}

	check := func(name string, root *FlameNode, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(root.Children) != 1 || root.Children[0].Name != "root" {
			t.Fatalf("%s: top frames = %d, want single 'root'", name, len(root.Children))
		}
		leaves := root.Children[0].Children
		if len(leaves) != maxFlameStacks {
			t.Fatalf("%s: stacks = %d, want exactly maxFlameStacks=%d", name, len(leaves), maxFlameStacks)
		}
		for _, leaf := range leaves {
			if leaf.Value <= extra {
				t.Fatalf("%s: lightest stack %s (value %d) survived the cut, want only the heaviest kept",
					name, leaf.Name, leaf.Value)
			}
		}
		if root.Value != wantValue {
			t.Fatalf("%s: root value = %d, want %d (sum of the %d heaviest stacks)", name, root.Value, wantValue, maxFlameStacks)
		}
	}

	root, err := q.Flame(ctx, 31, "api", "", "cpu", "", now.Add(-time.Hour), now.Add(time.Minute))
	check("Flame", root, err)
	root, err = q.FlameForTrace(ctx, 31, "T")
	check("FlameForTrace", root, err)
}
