package hostmetric

import "testing"

// AllMetrics — канон эмиссии агента; потребители (hostcharts/evaluator)
// используют те же константы по именам. Тест фиксирует состав и отсутствие
// дублей: выпадение имени из AllMetrics молча сломало бы паритет агента.
func TestAllMetricsComplete(t *testing.T) {
	want := []string{
		CPUUtilization, CPULogicalCount, MemoryUtilization,
		FilesystemUtilization, DiskIO, NetworkIO,
		LoadAvg1m, LoadAvg5m, LoadAvg15m, ProcessesCount, Uptime,
	}
	got := AllMetrics()
	if len(got) != len(want) {
		t.Fatalf("AllMetrics: %d имён, ждали %d", len(got), len(want))
	}
	seen := map[string]bool{}
	for i, name := range got {
		if name != want[i] {
			t.Errorf("AllMetrics[%d] = %q, ждали %q", i, name, want[i])
		}
		if seen[name] {
			t.Errorf("дубль %q", name)
		}
		seen[name] = true
	}
}

func TestExclusionListsNonEmpty(t *testing.T) {
	if len(ExcludedFSTypes) == 0 || len(ExcludedMountPrefixes) == 0 {
		t.Fatal("списки исключений ФС пусты — порог диска заорёт на snap/tmpfs")
	}
	for _, p := range ExcludedMountPrefixes {
		if p == "" || p[0] != '/' {
			t.Errorf("префикс маунта %q не абсолютный", p)
		}
	}
}
