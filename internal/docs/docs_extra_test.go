package docs

import "testing"

// TestRenderCacheHit — второй рендер той же страницы обслуживается из кеша.
// Проверяем, что результат идентичен первому (та же html/title) и ok=true —
// это покрывает ветку чтения из cache в Render.
func TestRenderCacheHit(t *testing.T) {
	h1, t1, ok1 := Render("ru", "installation")
	if !ok1 {
		t.Fatal("первый Render installation: ok=false")
	}
	h2, t2, ok2 := Render("ru", "installation")
	if !ok2 {
		t.Fatal("повторный Render installation (кеш): ok=false")
	}
	if h1 != h2 || t1 != t2 {
		t.Fatalf("кеш вернул иной результат: title %q vs %q", t1, t2)
	}
}

// TestRenderEnglishLocale — явная английская локаль читает en/*.md напрямую
// (без ru-fallback) и отдаёт непустой заголовок.
func TestRenderEnglishLocale(t *testing.T) {
	_, title, ok := Render("en", "getting-started")
	if !ok {
		t.Fatal("Render en getting-started: ok=false")
	}
	if title == "" {
		t.Fatal("пустой title у английской страницы")
	}
}

// TestFirstH1NoHeading — файл без "# " заголовка даёт пустую строку, а не панику
// и не мусор. Это ветка `return ""` в конце firstH1.
func TestFirstH1NoHeading(t *testing.T) {
	if got := firstH1([]byte("нет заголовка\nпросто текст\n## подзаголовок")); got != "" {
		t.Fatalf("firstH1 без H1 = %q, want пустую строку", got)
	}
}

// TestFirstH1SkipsLeadingContentAndPicksFirst — H1 берётся первым по порядку,
// даже если ему предшествует текст, и обрезаются пробелы.
func TestFirstH1SkipsLeadingContentAndPicksFirst(t *testing.T) {
	data := []byte("вводная строка\n\n#   Настоящий заголовок  \n\n# Второй\n")
	if got := firstH1(data); got != "Настоящий заголовок" {
		t.Fatalf("firstH1 = %q, want %q", got, "Настоящий заголовок")
	}
}

// TestPagesTitlesMatchRender — заголовки из Pages совпадают с тем, что отдаёт
// Render для той же страницы: реестр и рендер согласованы по H1.
func TestPagesTitlesMatchRender(t *testing.T) {
	for _, p := range Pages("ru") {
		_, title, ok := Render("ru", p.Slug)
		if !ok {
			t.Fatalf("Render ru %q: ok=false", p.Slug)
		}
		if title != p.Title {
			t.Fatalf("slug %q: Pages.Title=%q, Render.title=%q", p.Slug, p.Title, title)
		}
	}
}
