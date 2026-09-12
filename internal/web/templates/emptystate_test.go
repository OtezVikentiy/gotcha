package templates

import (
	"context"
	"strings"
	"testing"
)

func TestEmptyStateHeadingLevel(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		level int
		want  string
	}{
		{2, `<h2 class="empty-title"`},
		{3, `<h3 class="empty-title"`},
	} {
		var sb strings.Builder
		if err := emptyState("bug", "issues.empty.title", "issues.empty.body", "", "", tc.level).Render(ctx, &sb); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sb.String(), tc.want) {
			t.Errorf("level %d: в разметке нет %s", tc.level, tc.want)
		}
	}
}
