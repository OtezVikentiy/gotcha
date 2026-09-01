package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// TestTraceIsNeverServed — TRACE не обслуживается ни в одной конфигурации
// корневого mux и не отражает запрос обратно (XST).
//
// Режимов два, и они чисты ПО-РАЗНОМУ. В "web"/"all" catch-all веб-слоя
// (web.go:873) перехватывает любой метод раньше, чем stdlib успевает отдать
// 405, — приложение отвечает стилизованной 404 и Allow не отдаёт вовсе.
// В "ingest" веб-хендлера нет, и mux штатно отвечает 405 с Allow:
// "OPTIONS, POST" — OPTIONS в этот список добавляет сам stdlib, TRACE не
// добавляет никто, потому что он нигде не зарегистрирован.
// Поэтому ассерт «строго 404» или «строго 405» был бы верен ровно в половине
// конфигураций: проверяем то, что верно в обеих — ответ не 200, тело не
// отражает запрос, а Allow, если он вообще отдан, не содержит TRACE.
func TestTraceIsNeverServed(t *testing.T) {
	ingestHandler := ingest.NewHandler(nil, nil, nil, 1<<20)
	var metrics selfmetrics.Registry

	webHandler := web.New(nil, nil, nil, nil, "http://localhost:8080")

	for _, tc := range []struct {
		name string
		deps rootDeps
		path string
	}{
		{
			name: "web",
			deps: rootDeps{pg: fakePinger{}, ch: fakePinger{}, selfMetrics: &metrics,
				ingestHandler: ingestHandler, webHandler: webHandler},
			path: "/login",
		},
		{
			name: "ingest",
			deps: rootDeps{pg: fakePinger{}, ch: fakePinger{}, selfMetrics: &metrics,
				ingestHandler: ingestHandler},
			path: "/api/7/store/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := newRootMux(tc.deps)
			req := httptest.NewRequest("TRACE", tc.path, nil)
			req.Header.Set("X-Xst-Probe", "reflect-me-if-you-can")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Errorf("TRACE %s: код 200 — метод обслуживается", tc.path)
			}
			if strings.Contains(rec.Body.String(), "reflect-me-if-you-can") {
				t.Errorf("TRACE %s: тело ответа отражает заголовок запроса (XST)", tc.path)
			}
			if allow := rec.Header().Get("Allow"); strings.Contains(strings.ToUpper(allow), "TRACE") {
				t.Errorf("TRACE %s: Allow = %q содержит TRACE", tc.path, allow)
			}
		})
	}
}
