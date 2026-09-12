package templates

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/flashctx"
)

func renderWithFlash(t *testing.T, c templ.Component, f *flashctx.Flash) string {
	t.Helper()
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	if f != nil {
		ctx = flashctx.With(ctx, f)
	}
	var sb strings.Builder
	if err := c.Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func TestFlashView(t *testing.T) {
	ok := renderWithFlash(t, flashView(), &flashctx.Flash{Kind: "ok", Key: "flash.saved"})
	if !strings.Contains(ok, "Сохранено") {
		t.Errorf("сообщение не отрисовано: %s", ok)
	}
	if strings.Contains(ok, "flash--warn") {
		t.Error("обычное сообщение не должно быть предупреждающим")
	}
	// Крестик закрытия обязателен: без него сообщение висит до таймера.
	if !strings.Contains(ok, "flash-close") {
		t.Error("нет кнопки закрытия")
	}
	// role=status + aria-live: сообщение появляется после перезагрузки, и
	// пользователь скринридера должен его услышать, не потеряв фокус.
	if !strings.Contains(ok, `role="status"`) || !strings.Contains(ok, `aria-live="polite"`) {
		t.Error("сообщение должно объявляться вспомогательным технологиям")
	}

	warn := renderWithFlash(t, flashView(), &flashctx.Flash{Kind: "warn", Key: "flash.nothing_selected"})
	if !strings.Contains(warn, "flash--warn") {
		t.Error("предупреждение должно отличаться оформлением")
	}

	plural := renderWithFlash(t, flashView(), &flashctx.Flash{Kind: "ok", Key: "flash.issues_resolved", N: 5})
	if !strings.Contains(plural, "5") {
		t.Errorf("число не попало в сообщение: %s", plural)
	}

	none := renderWithFlash(t, flashView(), nil)
	if strings.Contains(none, "flash") {
		t.Errorf("без сообщения ничего рисовать не нужно: %s", none)
	}
}

func TestCardinalityNoticeView(t *testing.T) {
	out := renderTo(t, cardinalityNoticeView([]CardinalityNotice{{
		Field:     "transaction name",
		Limit:     10000,
		Collapsed: 47213,
		Samples:   []string{"GET /users/8812/profile", "GET /users/8813/profile"},
	}}))

	if !strings.Contains(out, "transaction name") || !strings.Contains(out, "10000") {
		t.Errorf("не указано поле и потолок: %s", out)
	}
	if !strings.Contains(out, "47213") {
		t.Errorf("не указано число схлопнутого: %s", out)
	}
	for _, sample := range []string{"GET /users/8812/profile", "GET /users/8813/profile"} {
		if !strings.Contains(out, sample) {
			t.Errorf("нет примера %q — без примеров диагностировать нечем", sample)
		}
	}
	if !strings.Contains(out, "/docs/cardinality") {
		t.Error("нет ссылки на инструкцию по исправлению")
	}

	// Поле без примеров всё равно рисуется (счётчик полезен сам по себе), но
	// без пустого списка.
	noSamples := renderTo(t, cardinalityNoticeView([]CardinalityNotice{{Field: "environment", Limit: 100, Collapsed: 3}}))
	if strings.Contains(noSamples, "<ul") {
		t.Error("пустой список примеров рисовать не нужно")
	}

	if out := renderTo(t, cardinalityNoticeView(nil)); strings.Contains(out, "cardinality-notice") {
		t.Errorf("без предупреждений блок не нужен: %s", out)
	}
}

// модалка держится классом, а не :target — переход на «#» её не закрывал вовсе.
func TestModalServerOpen(t *testing.T) {
	open := renderTo(t, createModalOpen("new-rule", "modal.close", false, true))
	if !strings.Contains(open, "modal--open") {
		t.Error("серверно открытая модалка не помечена классом")
	}
	if !strings.Contains(open, `id="new-rule-close"`) {
		t.Error("нет якоря закрытия — модалку нельзя будет закрыть")
	}
	if strings.Count(open, `href="#new-rule-close"`) != 2 {
		t.Errorf("крестик и фон должны вести на якорь закрытия: %s", open)
	}

	closed := renderTo(t, createModalOpen("new-rule", "modal.close", false, false))
	if strings.Contains(closed, "modal--open") {
		t.Error("обычная модалка не должна открываться сама")
	}
	if !strings.Contains(closed, `id="new-rule-close"`) {
		t.Error("якорь закрытия должен быть всегда")
	}
}
