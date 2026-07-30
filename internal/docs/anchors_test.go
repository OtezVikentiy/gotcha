package docs_test

import (
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
)

// TestHeadingsHaveAnchors: без id у заголовков якорных ссылок не существует.
//
// Тексты документации уже ссылаются на разделы прозой («см. раздел о внешних
// получателях ниже»), а браузерная ссылка на конкретный раздел не работала ни
// на одной странице: goldmark без WithAutoHeadingID заголовкам id не ставит.
func TestHeadingsHaveAnchors(t *testing.T) {
	for _, locale := range []string{"ru", "en"} {
		for _, slug := range []string{"sdk", "configuration", "privacy", "upgrade"} {
			html, _, ok := docs.Render(locale, slug)
			if !ok {
				t.Fatalf("нет страницы %s/%s", locale, slug)
			}
			if !strings.Contains(html, `<h2 id="`) {
				t.Errorf("%s/%s: у заголовков второго уровня нет id — ссылка на раздел не работает",
					locale, slug)
			}
		}
	}
}

// TestAnchorsAreMeaningfulAndStable: якорь кириллического заголовка обязан
// зависеть от ТЕКСТА, а не от номера раздела.
//
// Штатный генератор goldmark оставляет от кириллицы «heading-3»: вставка абзаца
// в середину страницы сдвигает нумерацию и протухает все ссылки ниже.
func TestAnchorsAreMeaningfulAndStable(t *testing.T) {
	html, _, ok := docs.Render("ru", "configuration")
	if !ok {
		t.Fatal("нет страницы ru/configuration")
	}
	if strings.Contains(html, `id="heading`) {
		t.Errorf("якоря вида heading-N: ссылка привязана к порядку разделов, а не к тексту")
	}
	// «Security (безопасность)» → security-bezopasnost.
	if !strings.Contains(html, `id="security-bezopasnost"`) {
		t.Errorf("нет ожидаемого якоря security-bezopasnost — транслитерация не работает")
	}
}

// TestSlugifyTransliterates закрепляет разбор: якорь виден в адресной строке,
// и «что это за раздел» должно читаться по нему.
func TestSlugifyTransliterates(t *testing.T) {
	cases := map[string]string{
		"Security (безопасность)":  "security-bezopasnost",
		"Хранение данных":          "hranenie-dannyh",
		"OAuth / SSO":              "oauth-sso",
		"Ёлки, объём и всё прочее": "elki-obem-i-vse-prochee",
		"   ": "",
	}
	for in, want := range cases {
		if got := docs.SlugifyForTest(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
