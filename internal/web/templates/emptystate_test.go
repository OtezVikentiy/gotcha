package templates

import (
	"context"
	"strings"
	"testing"
)

// TestEmptyStateHeadingLevel: заголовок пустого состояния продолжает структуру
// страницы (после <h1> — h2, внутри секции с <h2>/<h3> — h3), а не
// фиксированный <h3>, из-за которого структура прыгала h1→h3 (№78,
// WCAG 1.3.1).
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
