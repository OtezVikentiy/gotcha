package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

func TestRenderErrorLeaksNothing(t *testing.T) {
	h := &Handler{}
	for _, status := range []int{404, 500} {
		rec := httptest.NewRecorder()
		h.renderError(rec, httptest.NewRequest("GET", "/whatever", nil), status, "boom")
		body := rec.Body.String()

		if rec.Code != status {
			t.Errorf("status = %d, want %d", rec.Code, status)
		}
		if v := version.Version(); v != "" && strings.Contains(body, v) {
			t.Errorf("страница ошибки %d содержит версию сборки %q", status, v)
		}
		for _, marker := range []string{"goroutine ", ".go:", "runtime.", "panic:"} {
			if strings.Contains(body, marker) {
				t.Errorf("страница ошибки %d содержит след стека %q", status, marker)
			}
		}
	}
}

// notFound/generic renderError передавали в msg то же значение, что и заголовок
// (категорию), из-за чего написанная подсказка error.404.body/error.500.body не
// показывалась никогда, а на 404 заголовок и текст под ним дублировали друг друга.
func TestNotFoundShowsExplanationNotDuplicatedTitle(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest("GET", "/whatever", nil)
	rec := httptest.NewRecorder()
	h.notFound(rec, req)
	body := rec.Body.String()

	wantExplanation := i18n.T(req.Context(), "error.404.body")
	if !strings.Contains(body, wantExplanation) {
		t.Fatalf("страница 404 не показывает подсказку error.404.body: %q", wantExplanation)
	}
	title := i18n.T(req.Context(), "error.404.title")
	if strings.Count(body, title) != 1 {
		t.Errorf("заголовок %q должен встречаться один раз, встретился %d — текст под ним задваивает заголовок", title, strings.Count(body, title))
	}
}

func TestRenderErrorInternalShowsExplanation(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest("GET", "/whatever", nil)
	rec := httptest.NewRecorder()
	h.renderError(rec, req, 500, "")
	body := rec.Body.String()

	wantExplanation := i18n.T(req.Context(), "error.500.body")
	if !strings.Contains(body, wantExplanation) {
		t.Fatalf("страница 500 не показывает подсказку error.500.body: %q", wantExplanation)
	}
}
