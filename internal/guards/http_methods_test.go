package guards

import (
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

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
