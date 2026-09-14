package ingestsignal

import "testing"

// Список — буквальный дубль, не проекция resetWindow на себя: тест обязан
// заметить пропавшую или обнулённую запись, а не только совпасть с картой.
func TestResetWindowCoversAllKinds(t *testing.T) {
	kinds := []Kind{
		KindDeprecatedLogs, KindDeprecatedPprof, KindDeprecatedDeployments,
		KindKeyInvalid, KindKeyProjectMismatch, KindKeyScope,
	}
	if len(kinds) != len(resetWindow) {
		t.Fatalf("resetWindow содержит %d видов, ожидалось %d", len(resetWindow), len(kinds))
	}
	for _, k := range kinds {
		if resetWindow[k] <= 0 {
			t.Errorf("resetWindow[%s] = %v, want > 0", k, resetWindow[k])
		}
	}
}
