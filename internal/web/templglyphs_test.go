package web_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNoTextGlyphsInsteadOfIcons(t *testing.T) {
	files, err := filepath.Glob("templates/*.templ")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("не найдено ни одного .templ — проверь рабочую директорию теста")
	}

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // комментарии описывают UI словами, глиф в них уместен
			}
			for glyph, want := range map[string]string{
				"←": `@icon("arrow-left")`,
				"✓": `@icon("check")`,
				"○": `@icon("circle")`,
			} {
				if strings.Contains(line, glyph) {
					t.Errorf("%s:%d: текстовый глиф %q в разметке — нужен %s: %s",
						f, i+1, glyph, want, strings.TrimSpace(line))
				}
			}
		}
	}
}

func TestSpriteHasIconsUsedInMarkup(t *testing.T) {
	sprite, err := os.ReadFile("templates/icons.templ")
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("templates/*.templ")
	if err != nil {
		t.Fatal(err)
	}
	used := regexp.MustCompile(`@icon\("([a-z-]+)"\)`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range used.FindAllStringSubmatch(string(src), -1) {
			if !strings.Contains(string(sprite), `id="i-`+m[1]+`"`) {
				t.Errorf("%s: @icon(%q) — в спрайте нет символа i-%s", f, m[1], m[1])
			}
		}
	}
}

func TestNoArrowGlyphsInCSSContent(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(css), "\n") {
		if !strings.Contains(line, "content:") {
			continue
		}
		for _, glyph := range []string{"▸", "▾", "▶", "▼", "←", "→", "✓", "○"} {
			if strings.Contains(line, glyph) {
				t.Errorf("глиф %q в content: — нужна иконка спрайта: %s",
					glyph, strings.TrimSpace(line))
			}
		}
	}
}
