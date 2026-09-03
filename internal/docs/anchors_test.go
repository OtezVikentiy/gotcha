package docs_test

import (
	"regexp"
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

// TestInPageAnchorLinksResolve: каждая ссылка вида [текст](#якорь) внутри
// страницы обязана указывать на существующий id заголовка.
//
// Якоря кириллических заголовков ТРАНСЛИТЕРИРУЮТСЯ (см. slugify), а не
// повторяют текст заголовка, — и написанная «по смыслу» ссылка
// `(#удаление-и-автоматическая-очистка)` уезжает в percent-encoding и не ведёт
// никуда. Молча: markdown-ссылка на несуществующий якорь — не ошибка сборки, и
// автор правки замечает её, только если сам щёлкнет по ней в браузере.
// Проверка идёт по ВСЕМ страницам обеих локалей, а не по списку известных.
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

// TestCrossPageLinksResolve: каждая ссылка вида [текст](/docs/slug) или
// [текст](/docs/slug#якорь) обязана вести на страницу, которая реально есть
// в registry текущей локали, а якорь (если указан) — на существующий id
// заголовка ЦЕЛЕВОЙ страницы той же локали.
//
// TestInPageAnchorLinksResolve проверяет только ссылки на якоря ВНУТРИ той же
// страницы (`href="#..."`) — опечатка в slug'е соседней страницы или в её
// якоре (например, `/docs/upgrade` → `/docs/upgrad`, или правильный slug с
// протухшим `#якорем`) им не ловится и не роняет сборку: markdown-ссылка на
// несуществующую страницу — не ошибка компиляции, автор замечает её только
// щёлкнув по ней в браузере. Целевую страницу рендерим через docs.Render —
// это даёт готовый список id заголовков (транслитерация уже применена) без
// повторной реализации slugify здесь.
func TestCrossPageLinksResolve(t *testing.T) {
	// Якорь захватывается ЛЮБЫМИ символами до закрывающей кавычки, а не
	// закрытым классом `[a-z0-9-]+`: закрытый класс не матчится на опечатке
	// вида `#security-bezopasnostX` (заглавная буква не входит в класс), и
	// такая ссылка просто выпадает из FindAllStringSubmatch — регэксп молча
	// пропускает битую ссылку вместо того, чтобы её проверить и завалить тест.
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
