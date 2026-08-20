package escalation_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRecoveryChannelsDistinctAndScoped — RecoveryChannels обязана вернуть
// DISTINCT каналы, видевшие эскалацию ИМЕННО этого (source, incident_id):
// один канал, залогированный на нескольких ступенях, должен войти в
// результат один раз, а строки другого инцидента и другого источника с тем
// же incident_id — не подмешаться (M-7 брифа Task 6: recovery адресуется
// только тем, кто реально видел тревогу этого инцидента).
func TestRecoveryChannelsDistinctAndScoped(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)
	c3 := newChannel(t, pool, pid, true)

	const incidentID = int64(9001)

	// Целевой инцидент 'host'/9001: c1 залогирован дважды на разных ступенях
	// (должен схлопнуться в один элемент через DISTINCT), c2 — один раз.
	if err := escalation.LogStep(ctx, pool, "host", incidentID, c1, 0); err != nil {
		t.Fatalf("LogStep c1 step0: %v", err)
	}
	if err := escalation.LogStep(ctx, pool, "host", incidentID, c1, 1); err != nil {
		t.Fatalf("LogStep c1 step1: %v", err)
	}
	if err := escalation.LogStep(ctx, pool, "host", incidentID, c2, 1); err != nil {
		t.Fatalf("LogStep c2 step1: %v", err)
	}

	// Шум: тот же incident_id, но другой source — не должен попасть в выборку.
	if err := escalation.LogStep(ctx, pool, "metric", incidentID, c3, 0); err != nil {
		t.Fatalf("LogStep noise other-source: %v", err)
	}
	// Шум: тот же source, но другой incident_id — не должен попасть в выборку.
	if err := escalation.LogStep(ctx, pool, "host", incidentID+1, c3, 0); err != nil {
		t.Fatalf("LogStep noise other-incident: %v", err)
	}

	got, err := escalation.RecoveryChannels(ctx, pool, "host", incidentID)
	if err != nil {
		t.Fatalf("RecoveryChannels: %v", err)
	}

	seen := map[int64]int{}
	for _, ch := range got {
		seen[ch]++
	}
	if len(got) != 2 {
		t.Fatalf("RecoveryChannels returned %d channels %v, want 2 (DISTINCT c1,c2)", len(got), got)
	}
	if seen[c1] != 1 {
		t.Errorf("c1 count = %d, want 1 (DISTINCT must collapse duplicate steps)", seen[c1])
	}
	if seen[c2] != 1 {
		t.Errorf("c2 count = %d, want 1", seen[c2])
	}
	if seen[c3] != 0 {
		t.Errorf("c3 must not appear (belongs to other source/incident), got count %d", seen[c3])
	}
}
