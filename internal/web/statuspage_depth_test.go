package web

import "testing"

// TestStatusPageDepthFollowsRetention: публичная страница не должна обещать
// больше истории, чем реально хранится.
//
// TTL check_results едет на общей ручке GOTCHA_EVENT_RETENTION_DAYS: уменьшив её до
// 30, оператор молча укорачивал публичную историю, а страница продолжала
// рисовать 90 клеток и подписывать их «за 90 дней» — пустые клетки читаются как
// «мониторинг не работал», а не как «так настроено хранение».
func TestStatusPageDepthFollowsRetention(t *testing.T) {
	cases := []struct {
		retention int
		want      int
	}{
		{0, statusPageBuckets},   // срок не задан — полная глубина
		{90, statusPageBuckets},  // ровно предел
		{120, statusPageBuckets}, // больше предела — предел
		{30, 30},                 // короче предела — по сроку хранения
		{1, 1},
	}
	for _, tc := range cases {
		h := &Handler{RetentionDays: tc.retention}
		if got := h.statusPageDays(); got != tc.want {
			t.Errorf("RetentionDays=%d: глубина %d, want %d", tc.retention, got, tc.want)
		}
	}
}
