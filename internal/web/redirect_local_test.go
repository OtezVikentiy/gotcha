package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// redirectLocal проверяет ещё раз у заголовка независимо от safeNextPath — тест закрепляет
// именно эту вторую проверку, а не полагается на первую.
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
		// Браузер выбрасывает табы/переводы строк из адреса ДО разбора (WHATWG URL), так что
		// «/<TAB>/evil» доезжает как «//evil» — протокол-относительный адрес чужого хоста.
		{"таб внутри пути отвергается", "/\t/evil.example/x", "/"},
		{"перевод строки внутри пути отвергается", "/\n/evil.example/x", "/"},
		{"возврат каретки внутри пути отвергается", "/\r/evil.example/x", "/"},
		{"таб перед обратной косой отвергается", "/\t\\evil.example/x", "/"},
		{"NUL внутри пути отвергается", "/\x00/evil.example/x", "/"},
		{"DEL внутри пути отвергается", "/\x7f/evil.example/x", "/"},
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
		"/\t/evil.example", "/\n/evil.example", "/\r/evil.example",
		"/\x00/evil.example", "/\x7f/evil.example", "/ /evil.example",
	}
	for _, raw := range raws {
		next := safeNextPath(raw)
		if next != "" && !isLocalPath(next) {
			t.Errorf("safeNextPath(%q) = %q — принято разборщиком, но отвергается у Location", raw, next)
		}
	}
}
