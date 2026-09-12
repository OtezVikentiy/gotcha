package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

func TestErrorPageShowsGivenReason(t *testing.T) {
	const reason = "для этого адреса нет приглашения"
	html := renderTo(t, ErrorPage(403, reason, ""))
	if !strings.Contains(html, reason) {
		t.Fatalf("страница 403 не содержит переданную причину %q", reason)
	}
}

// ожидаемое значение берётся из каталога, а не зашито литералом: перевод
// текста — дело переводчика, а не повод для ложного красного здесь.
func TestErrorPageWithoutReasonKeepsTemplateText(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantBody := i18n.T(ctx, errorBodyKey(404))
	html := renderTo(t, ErrorPage(404, "", ""))
	if !strings.Contains(html, wantBody) {
		t.Fatalf("страница 404 без причины должна показывать шаблонный текст (error.404.body): %q", wantBody)
	}
}
