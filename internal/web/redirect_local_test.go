package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Location после входа строится из поля формы «next». safeNextPath разбирает
// его на входе, redirectLocal проверяет ещё раз у самого заголовка — этот тест
// закрепляет вторую проверку: она обязана держать инвариант в одиночку, потому
// что смысл её существования в том, чтобы не зависеть от вызова, сделанного
// строкой выше.
func TestRedirectLocalKeepsRedirectOnSite(t *testing.T) {
	cases := []struct {
		name string
		dest string
		want string
	}{
		{"пусто — на главную", "", "/"},
		{"свой путь сохраняется", "/projects/7/issues", "/projects/7/issues"},
		{"строка запроса сохраняется", "/issues?status=resolved&page=2", "/issues?status=resolved&page=2"},
		{"абсолютный адрес отвергается", "https://evil.example/x", "/"},
		{"протокол-относительный отвергается", "//evil.example/x", "/"},
		{"обратная косая отвергается", "/\\evil.example/x", "/"},
		{"адрес без ведущей косой отвергается", "evil.example/x", "/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/login", nil)
			redirectLocal(w, r, c.dest)
			if got := w.Code; got != http.StatusSeeOther {
				t.Errorf("код %d, want %d", got, http.StatusSeeOther)
			}
			if got := w.Header().Get("Location"); got != c.want {
				t.Errorf("Location = %q, want %q", got, c.want)
			}
		})
	}
}

// safeNextPath и redirectLocal обязаны отвергать одно и то же: расхождение
// между ними означало бы, что один из двух заслонов лишний.
func TestSafeNextPathAgreesWithRedirectLocal(t *testing.T) {
	raws := []string{
		"", "/", "/projects/7", "//evil.example", "/\\evil.example",
		"https://evil.example", "evil.example", "javascript:alert(1)",
	}
	for _, raw := range raws {
		next := safeNextPath(raw)
		if next != "" && !isLocalPath(next) {
			t.Errorf("safeNextPath(%q) = %q — принято разборщиком, но отвергается у Location", raw, next)
		}
	}
}
