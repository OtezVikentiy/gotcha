package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestErrorPageShowsGivenReason: восемь разных отказов при первом входе через
// провайдера («для этого email нет приглашения», «email не из домена
// организации», «провайдер не подтвердил email») печатались одинаковым
// шаблонным текстом по статусу — вычисленная причина отбрасывалась, и человек
// не понимал, что делать дальше.
func TestErrorPageShowsGivenReason(t *testing.T) {
	const reason = "для этого адреса нет приглашения"
	html := renderTo(t, ErrorPage(403, reason, ""))
	if !strings.Contains(html, reason) {
		t.Fatalf("страница 403 не содержит переданную причину %q", reason)
	}
}

// TestErrorPageWithoutReasonKeepsTemplateText: без причины страница
// по-прежнему осмысленна — общий текст по статусу никуда не девается.
//
// Ожидаемое значение берётся из каталога (i18n.T с тем же ключом, что и сам
// шаблон), а не зашито русским литералом: перевод текста error.404.body —
// дело переводчика, а не повод для ложного красного в этом тесте. Тест
// по-прежнему бьётся, если шаблон перестанет печатать текст по статусу
// (см. раздел «Раунд правок 1» отчёта задачи).
func TestErrorPageWithoutReasonKeepsTemplateText(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantBody := i18n.T(ctx, errorBodyKey(404))
	html := renderTo(t, ErrorPage(404, "", ""))
	if !strings.Contains(html, wantBody) {
		t.Fatalf("страница 404 без причины должна показывать шаблонный текст (error.404.body): %q", wantBody)
	}
}
