package profile_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestWriterCollapsesAndFlushes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	w := profile.NewWriter(conn)
	go w.Run()
	f := func(n string) profile.Frame { return profile.Frame{Function: n} }
	w.Add(3, profile.Profile{Type: "cpu", Service: "api", TraceID: "trace-xyz", Timestamp: time.Now().UTC(), Samples: []profile.Sample{
		{Stack: []profile.Frame{f("root"), f("a")}, Value: 2},
		{Stack: []profile.Frame{f("root"), f("a")}, Value: 3}, // тот же стек → схлоп в 5
		{Stack: []profile.Frame{f("root"), f("b")}, Value: 1},
	}})
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	ctx := context.Background()
	var rows uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM profile_samples WHERE project_id=3").Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2 {
		t.Fatalf("rows = %d, want 2 (collapsed)", rows)
	}
	var v uint64
	if err := conn.QueryRow(ctx,
		"SELECT value FROM profile_samples WHERE project_id=3 AND stack=['root','a']").Scan(&v); err != nil {
		t.Fatalf("select collapsed: %v", err)
	}
	if v != 5 {
		t.Fatalf("collapsed value = %d, want 5", v)
	}
	var tid string
	if err := conn.QueryRow(ctx, "SELECT any(trace_id) FROM profile_samples WHERE project_id=3").Scan(&tid); err != nil || tid != "trace-xyz" {
		t.Fatalf("trace_id = %q err=%v, want trace-xyz", tid, err)
	}
}
