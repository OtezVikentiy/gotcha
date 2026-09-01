package guards

import (
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// TestWebRoutesRegisterOnlyGetAndPost — ни один маршрут веб-слоя не
// регистрируется под методом вне {GET, POST}. Это и закрывает XST: stdlib
// строит заголовок Allow ИСКЛЮЧИТЕЛЬНО из зарегистрированных методов, и пока
// TRACE нигде не зарегистрирован, он в Allow не попадёт ни при какой форме
// mux'а. Поведение держится на выборе роутера, а не на явном коде, — поэтому
// сторож: первая же регистрация вида `inner.HandleFunc("TRACE /debug", …)`
// или "PUT /..." обязана быть замечена в ревью, а не в отчёте сканера.
//
// Uptime проставляется ненулевым: пять публичных маршрутов (heartbeat, probe
// lease/results, статус-страница) регистрируются под `if h.Uptime != nil`
// (web.go:850) и на голом web.New(nil, …) под сторожа не попали бы вовсе.
// Сервисы при этом не используются — Register только регистрирует.
func TestWebRoutesRegisterOnlyGetAndPost(t *testing.T) {
	h := web.New(nil, nil, nil, nil, "http://localhost:8080")
	h.Uptime = &uptime.Service{}
	h.Register(http.NewServeMux())

	routes := h.RegisteredRoutes()
	if len(routes) == 0 {
		t.Fatal("RegisteredRoutes() пуст — сторож проверяет пустоту вместо маршрутов")
	}
	for _, route := range routes {
		method, path, ok := strings.Cut(route, " ")
		if !ok {
			// Catch-all "/" регистрируется без метода — он и обслуживает
			// любой метод, отдавая стилизованную 404 (web.go:873).
			continue
		}
		switch method {
		case "GET", "POST":
		default:
			t.Errorf("маршрут %q зарегистрирован под методом %q: приложение обслуживает только GET и POST, "+
				"а любой лишний метод попадает в Allow и подсвечивает поверхность сканеру", path, method)
		}
	}
}
