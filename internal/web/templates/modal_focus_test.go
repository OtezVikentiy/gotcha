package templates

import (
	"context"
	"strings"
	"testing"
)

func TestModalHeadingFocusable(t *testing.T) {
	var sb strings.Builder
	if err := createModalOpen("m1", "modal.close", false, false).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `id="m1-title" tabindex="-1"`) {
		t.Errorf("заголовок модалки не принимает программный фокус: %s", sb.String())
	}
}
