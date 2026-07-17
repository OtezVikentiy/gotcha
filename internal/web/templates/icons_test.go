package templates

import (
	"context"
	"strings"
	"testing"
)

// TestIconRendersUseRef — icon(name) должен рендерить <use href="#i-<name>">,
// ссылающийся на символ из iconSprite() (см. layoutBody, где спрайт
// рендерится первым потомком <body>).
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

// TestIconSpriteContainsSymbols — базовая проверка, что спрайт содержит
// символы для навигационного набора иконок.
func TestIconSpriteContainsSymbols(t *testing.T) {
	var sb strings.Builder
	if err := iconSprite().Render(context.Background(), &sb); err != nil {
		t.Fatalf("render iconSprite: %v", err)
	}
	out := sb.String()
	for _, name := range []string{"bug", "zap", "chart", "activity", "bell", "building"} {
		if !strings.Contains(out, `id="i-`+name+`"`) {
			t.Fatalf("sprite missing symbol id=i-%s: %s", name, out)
		}
	}
	if strings.Contains(out, `style="display:none"`) {
		t.Fatalf("sprite must not use inline style (CSP): %s", out)
	}
}
