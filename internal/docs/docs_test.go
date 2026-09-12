package docs

import (
	"strings"
	"testing"
)

func TestPagesRegistryBothLocales(t *testing.T) {
	for _, loc := range []string{"ru", "en"} {
		pages := Pages(loc)
		if len(pages) != len(registry) {
			t.Fatalf("Pages(%q) = %d pages, want %d (registry size)", loc, len(pages), len(registry))
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
}

func TestRenderUnknownSlug(t *testing.T) {
	if _, _, ok := Render("ru", "does-not-exist"); ok {
		t.Fatal("unknown slug: ok=true, want false")
	}
}

func TestRenderLocaleFallback(t *testing.T) {
	if _, _, ok := Render("de", "glossary"); !ok {
		t.Fatal("locale fallback: ok=false")
	}
}
