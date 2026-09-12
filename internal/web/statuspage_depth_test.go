package web

import "testing"

func TestStatusPageDepthFollowsRetention(t *testing.T) {
	cases := []struct {
		retention int
		want      int
	}{
		{0, statusPageBuckets},
		{90, statusPageBuckets},
		{120, statusPageBuckets},
		{30, 30},
		{1, 1},
	}
	for _, tc := range cases {
		h := &Handler{RetentionDays: tc.retention}
		if got := h.statusPageDays(); got != tc.want {
			t.Errorf("RetentionDays=%d: глубина %d, want %d", tc.retention, got, tc.want)
		}
	}
}
