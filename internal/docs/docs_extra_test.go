package docs

import "testing"

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

func TestRenderEnglishLocale(t *testing.T) {
	_, title, ok := Render("en", "getting-started")
	if !ok {
		t.Fatal("Render en getting-started: ok=false")
	}
	if title == "" {
		t.Fatal("пустой title у английской страницы")
	}
}

func TestFirstH1NoHeading(t *testing.T) {
	if got := firstH1([]byte("нет заголовка\nпросто текст\n## подзаголовок")); got != "" {
		t.Fatalf("firstH1 без H1 = %q, want пустую строку", got)
	}
}

func TestFirstH1SkipsLeadingContentAndPicksFirst(t *testing.T) {
	data := []byte("вводная строка\n\n#   Настоящий заголовок  \n\n# Второй\n")
	if got := firstH1(data); got != "Настоящий заголовок" {
		t.Fatalf("firstH1 = %q, want %q", got, "Настоящий заголовок")
	}
}

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
