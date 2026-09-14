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

// Анонимному путь назад — «Войти», не «На главную»: анонимный не знает, что
// «На главную» его туда и приведёт. Вошедшему, наоборот, нужна именно главная.
func TestErrorPageOffersLoginOnlyWhenAnonymous(t *testing.T) {
	anon := renderTo(t, ErrorPage(403, "", ""))
	if !strings.Contains(anon, `<a class="btn btn-primary" href="/login">`) {
		t.Errorf("анонимному не предложена ссылка на вход: %s", anon)
	}
	if strings.Contains(anon, `<a class="btn btn-primary" href="/">`) {
		t.Errorf("анонимному предложена ссылка на главную вместо входа: %s", anon)
	}

	loggedIn := renderTo(t, ErrorPage(403, "", "u@e.com"))
	if !strings.Contains(loggedIn, `<a class="btn btn-primary" href="/">`) {
		t.Errorf("вошедшему не предложена ссылка на главную: %s", loggedIn)
	}
	if strings.Contains(loggedIn, `<a class="btn btn-primary" href="/login">`) {
		t.Errorf("вошедшему предложена ссылка на вход вместо главной: %s", loggedIn)
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
