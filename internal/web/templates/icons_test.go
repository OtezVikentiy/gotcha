package templates

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func TestIconRendersUseRef(t *testing.T) {
	var sb strings.Builder
	if err := icon("bug").Render(context.Background(), &sb); err != nil {
		t.Fatalf("render icon: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "#i-bug") {
		t.Fatalf("icon output missing #i-bug ref: %s", out)
	}
	if !strings.Contains(out, "<use") {
		t.Fatalf("icon output missing <use element: %s", out)
	}
}

func TestIconSpriteContainsSymbols(t *testing.T) {
	var sb strings.Builder
	if err := iconSprite().Render(context.Background(), &sb); err != nil {
		t.Fatalf("render iconSprite: %v", err)
	}
	out := sb.String()
	for _, name := range []string{"home", "bug", "zap", "chart", "activity", "bell", "building"} {
		if !strings.Contains(out, `id="i-`+name+`"`) {
			t.Fatalf("sprite missing symbol id=i-%s: %s", name, out)
		}
	}
	if strings.Contains(out, `style="display:none"`) {
		t.Fatalf("sprite must not use inline style (CSP): %s", out)
	}
	for _, name := range []string{"search", "x", "arrow-up", "arrow-down", "play", "pause"} {
		if strings.Contains(out, `id="i-`+name+`"`) {
			t.Errorf("спрайт всё ещё содержит неиспользуемый символ i-%s — ~719 Б на каждый ответ впустую", name)
		}
	}
}

// /login рендерит и chromeless-шапку (brandMark в topbar), и свой auth-brand —
// два brandMark() на одной странице обязаны получить разные id градиента.
func TestLoginPageHasNoDuplicateBrandGradID(t *testing.T) {
	out := renderTo(t, Login("", "", "", nil))
	idRe := regexp.MustCompile(`id="(brand-grad[\w-]*)"`)
	seen := map[string]int{}
	for _, m := range idRe.FindAllStringSubmatch(out, -1) {
		seen[m[1]]++
	}
	if len(seen) < 2 {
		t.Fatalf("ожидались минимум два разных brand-grad* id (topbar + auth-brand), получено %v: %s", seen, out)
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id=%q встречается %d раз на /login — разметка невалидна", id, n)
		}
	}
}
