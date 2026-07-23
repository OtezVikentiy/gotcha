package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// corsPreflight должен отвечать 204 без тела и с разрешающими заголовками —
// иначе браузер не отправит кросс-origin envelope (Web Vitals с чужого домена).
func TestCORSPreflight(t *testing.T) {
	rec := httptest.NewRecorder()
	corsPreflight(rec, httptest.NewRequest(http.MethodOptions, "/api/1/envelope/", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want %q", got, "*")
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("Allow-Methods пуст")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("тело preflight не пустое: %q", rec.Body.String())
	}
}

// cors обязан прокинуть вызов дальше И выставить Allow-Origin на реальном ответе,
// иначе браузер отбросит ответ ingest как кросс-origin.
func TestCORSWrapsHandler(t *testing.T) {
	called := false
	wrapped := cors(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	wrapped(rec, httptest.NewRequest(http.MethodPost, "/api/1/envelope/", nil))

	if !called {
		t.Fatal("обёрнутый обработчик не вызван")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want %q", got, "*")
	}
}
