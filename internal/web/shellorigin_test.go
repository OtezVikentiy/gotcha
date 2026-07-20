package web

import (
	"net/http/httptest"
	"testing"
)

// TestNavOriginPath — страница эндпойнта общая для «Транзакций» и «Web
// Vitals». Без пометки об источнике переход из Web Vitals подсвечивал бы
// «Транзакции», то есть пользователя молча уносило в соседний подраздел.
func TestNavOriginPath(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		// Обычный переход из списка транзакций — путь как есть (r.URL.Path
		// отдаёт его уже декодированным, навигация сверяется с ним же).
		{"/projects/7/performance/GET%20%2F", "/projects/7/performance/GET /"},
		// Переход из Web Vitals — подсветка остаётся на Web Vitals.
		{"/projects/7/performance/GET%20%2F?from=web-vitals", "/projects/7/web-vitals"},
		// Неизвестный источник игнорируется: значение приходит из адреса и
		// не должно влиять на навигацию.
		{"/projects/7/performance/GET%20%2F?from=whatever", "/projects/7/performance/GET /"},
		// Без проекта в пути подменять нечего.
		{"/docs?from=web-vitals", "/docs"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.url, nil)
		if got := navOriginPath(r); got != c.want {
			t.Errorf("navOriginPath(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestEndpointOrigin — в шаблон попадает только известное значение.
func TestEndpointOrigin(t *testing.T) {
	if got := endpointOrigin("web-vitals"); got != "web-vitals" {
		t.Errorf("endpointOrigin(web-vitals) = %q", got)
	}
	for _, in := range []string{"", "transactions", "<script>"} {
		if got := endpointOrigin(in); got != "" {
			t.Errorf("endpointOrigin(%q) = %q, ожидалась пустая строка", in, got)
		}
	}
}
