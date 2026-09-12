package docs_test

import (
	"regexp"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
)

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

func TestAnchorsAreMeaningfulAndStable(t *testing.T) {
	html, _, ok := docs.Render("ru", "configuration")
	if !ok {
		t.Fatal("нет страницы ru/configuration")
	}
	if strings.Contains(html, `id="heading`) {
		t.Errorf("якоря вида heading-N: ссылка привязана к порядку разделов, а не к тексту")
	}
	if !strings.Contains(html, `id="security-bezopasnost"`) {
		t.Errorf("нет ожидаемого якоря security-bezopasnost — транслитерация не работает")
	}
}

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

// Битая markdown-ссылка на якорь — не ошибка сборки: без этого теста её
// находят только руками, щёлкнув по ней в браузере.
func TestInPageAnchorLinksResolve(t *testing.T) {
	anchorLink := regexp.MustCompile(`href="#([^"]+)"`)
	for _, locale := range []string{"ru", "en"} {
		for _, p := range docs.Pages(locale) {
			html, _, ok := docs.Render(locale, p.Slug)
			if !ok {
				t.Fatalf("нет страницы %s/%s", locale, p.Slug)
			}
			for _, m := range anchorLink.FindAllStringSubmatch(html, -1) {
				if !strings.Contains(html, `id="`+m[1]+`"`) {
					t.Errorf("%s/%s: ссылка на якорь #%s, а заголовка с таким id на странице нет",
						locale, p.Slug, m[1])
				}
			}
		}
	}
}

func TestCrossPageLinksResolve(t *testing.T) {
	// Захват до закрывающей кавычки, не классом `[a-z0-9-]+`: закрытый класс
	// молча пропускает опечатку вида `#security-bezopasnostX` вместо провала теста.
	crossLink := regexp.MustCompile(`href="(/docs/[a-z0-9-]+)(?:#([^"]*))?"`)
	for _, locale := range []string{"ru", "en"} {
		pages := docs.Pages(locale)
		known := make(map[string]bool, len(pages))
		for _, p := range pages {
			known[p.Slug] = true
		}
		for _, p := range pages {
			html, _, ok := docs.Render(locale, p.Slug)
			if !ok {
				t.Fatalf("нет страницы %s/%s", locale, p.Slug)
			}
			for _, m := range crossLink.FindAllStringSubmatch(html, -1) {
				target, anchor := strings.TrimPrefix(m[1], "/docs/"), m[2]
				if !known[target] {
					t.Errorf("%s/%s: ссылка на /docs/%s, а такой страницы нет в registry",
						locale, p.Slug, target)
					continue
				}
				if anchor == "" {
					continue
				}
				targetHTML, _, ok := docs.Render(locale, target)
				if !ok {
					t.Errorf("%s/%s: ссылка на /docs/%s#%s, но целевая страница /docs/%s не рендерится",
						locale, p.Slug, target, anchor, target)
					continue
				}
				if !strings.Contains(targetHTML, `id="`+anchor+`"`) {
					t.Errorf("%s/%s: ссылка на /docs/%s#%s, а заголовка с таким id на странице /docs/%s нет",
						locale, p.Slug, target, anchor, target)
				}
			}
		}
	}
}
