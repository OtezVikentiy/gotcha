package docs

import (
	"strings"
	"testing"
)

func TestPagesRegistryBothLocales(t *testing.T) {
	for _, loc := range []string{"ru", "en"} {
		pages := Pages(loc)
		if len(pages) != 9 {
			t.Fatalf("Pages(%q) = %d pages, want 9", loc, len(pages))
		}
		for _, p := range pages {
			if p.Slug == "" || p.Title == "" {
				t.Fatalf("Pages(%q): empty slug/title: %+v", loc, p)
			}
		}
	}
}

func TestRenderKnownSlug(t *testing.T) {
	html, title, ok := Render("ru", "getting-started")
	if !ok {
		t.Fatal("Render ru getting-started: ok=false")
	}
	if title == "" {
		t.Fatal("empty title")
	}
	if !strings.Contains(html, "<h") {
		t.Fatalf("rendered html has no heading: %q", html[:min(80, len(html))])
	}
	// H1 не должен дублироваться в теле (используем его как заголовок страницы,
	// а goldmark рендерит его как <h1> — решение: рендерим H1 отдельно как title
	// и НЕ включаем в тело ИЛИ включаем; тест фиксирует выбор — см. Step 4).
}

func TestRenderUnknownSlug(t *testing.T) {
	if _, _, ok := Render("ru", "does-not-exist"); ok {
		t.Fatal("unknown slug: ok=true, want false")
	}
}

func TestRenderLocaleFallback(t *testing.T) {
	// Неизвестная локаль → ru-fallback, не паника, ok=true для валидного slug.
	if _, _, ok := Render("de", "glossary"); !ok {
		t.Fatal("locale fallback: ok=false")
	}
}
