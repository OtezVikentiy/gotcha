package templates

import (
	"context"
	"strings"
	"testing"
)

// TestModalHeadingFocusable: заголовок модалки несёт tabindex="-1" — цель
// программного фокуса для modal.js. Без него при серверном переоткрытии после
// ошибки валидации фокус остаётся в начале страницы, и пользователь
// клавиатуры модалку «не видит» (№80).
func TestModalHeadingFocusable(t *testing.T) {
	var sb strings.Builder
	if err := createModalOpen("m1", "modal.close", false, false).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	// Именно у заголовка: tabindex="-1" на фоне (modal-backdrop) есть и так,
	// общий Contains по нему был бы зелёным без правки.
	if !strings.Contains(sb.String(), `id="m1-title" tabindex="-1"`) {
		t.Errorf("заголовок модалки не принимает программный фокус: %s", sb.String())
	}
}
