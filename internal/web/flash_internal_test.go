package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/flashctx"
)

// TestFlashRoundTrip — сообщение переживает редирект и показывается ровно один
// раз: cookie гасится тем же запросом, которым читается.
//
// Именно поэтому cookie, а не query-параметр: параметр остаётся в адресе,
// залипает при F5 и уезжает в закладку.
func TestFlashRoundTrip(t *testing.T) {
	h := &Handler{BaseURL: "https://gotcha.example"}

	// Обработчик ставит сообщение перед редиректом.
	rec := httptest.NewRecorder()
	h.flashOK(rec, "flash.saved", 0)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != flashCookie {
		t.Fatalf("cookie не поставлена: %+v", cookies)
	}
	if !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Errorf("cookie должна быть HttpOnly и Secure на https-инстансе: %+v", cookies[0])
	}

	// Следующий запрос её читает, гасит и кладёт сообщение в контекст.
	var seen *flashctx.Flash
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = flashctx.FromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/projects/1/issues", nil)
	req.AddCookie(cookies[0])
	rec2 := httptest.NewRecorder()
	h.withFlash(next).ServeHTTP(rec2, req)

	if seen == nil || seen.Key != "flash.saved" || seen.Kind != "ok" {
		t.Fatalf("сообщение не доехало до обработчика: %+v", seen)
	}
	// Гашение: MaxAge<0 в ответе.
	var cleared bool
	for _, c := range rec2.Result().Cookies() {
		if c.Name == flashCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("cookie не погашена — сообщение показалось бы повторно при F5")
	}
}

// TestFlashRejectsForgedCookie — значение cookie полностью подконтрольно
// клиенту, поэтому в ней хранится КЛЮЧ из белого списка, а не текст. Иначе
// подсунутая ссылка рисовала бы произвольное сообщение на нашей же странице —
// готовая площадка для фишинга.
func TestFlashRejectsForgedCookie(t *testing.T) {
	forged := []string{
		"ok|Ваш+пароль+истёк,+введите+его+заново",
		"ok|flash.unknown_key",
		"evil|flash.saved",
		"flash.saved",
		"",
		"ok|flash.saved|not-a-number",
	}
	for _, v := range forged {
		f := parseFlash(v)
		switch v {
		case "ok|flash.saved|not-a-number":
			// Ключ валиден, мусорное число игнорируется.
			if f == nil || f.N != 0 {
				t.Errorf("валидный ключ с мусорным числом: %+v", f)
			}
		default:
			if f != nil {
				t.Errorf("подделка %q принята: %+v", v, f)
			}
		}
	}
}

// TestFlashPluralCarriesCount — число доезжает до формы множественного числа:
// «помечено решёнными 5 проблем», а не «помечено решёнными».
func TestFlashPluralCarriesCount(t *testing.T) {
	rec := httptest.NewRecorder()
	h := &Handler{BaseURL: "http://localhost:59080"}
	h.flashOK(rec, "flash.issues_resolved", 5)

	c := rec.Result().Cookies()[0]
	if c.Secure {
		t.Error("на http-инстансе Secure не ставится")
	}
	f := parseFlash(c.Value)
	if f == nil || f.N != 5 {
		t.Fatalf("число не доехало: %+v", f)
	}
	// Отрицательное число отбрасывается: форм множественного числа для него нет.
	if f := parseFlash("ok|flash.issues_resolved|-3"); f == nil || f.N != 0 {
		t.Errorf("отрицательное число должно игнорироваться: %+v", f)
	}
}

// TestFlashSkipsStatic — на статику middleware не тратится и, что важнее, не
// гасит cookie: иначе сообщение съедалось бы параллельным запросом за app.css
// раньше, чем отрисуется страница.
func TestFlashSkipsStatic(t *testing.T) {
	h := &Handler{BaseURL: "https://gotcha.example"}
	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: "ok%7Cflash.saved"})
	rec := httptest.NewRecorder()

	called := false
	h.withFlash(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		if flashctx.FromContext(r.Context()) != nil {
			t.Error("на статике сообщение читать не нужно")
		}
	})).ServeHTTP(rec, req)

	if !called {
		t.Fatal("запрос не пропущен дальше")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie && c.MaxAge < 0 {
			t.Error("статика погасила сообщение до того, как его показала страница")
		}
	}
}

// TestFlashUnknownKeyNotSet — обработчик не может поставить сообщение, которого
// нет в белом списке: опечатка в ключе не должна давать пустую плашку.
func TestFlashUnknownKeyNotSet(t *testing.T) {
	// Неизвестный ключ пишет error-лог (см. setFlash, задача 1) — это здесь не
	// предмет проверки (её ведёт TestSetFlashUnknownKeyIsLoud), поэтому лог
	// перехватывается, а не летит в реальный вывод прогона: без перехвата
	// строка ERROR попадала бы в вывод go test и выглядела бы как настоящий
	// сбой, хотя тест и так зелёный.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	(&Handler{}).flashOK(rec, "flash.typo", 0)
	if n := len(rec.Result().Cookies()); n != 0 {
		t.Errorf("неизвестный ключ поставил cookie (%d)", n)
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), flashCookie) {
		t.Error("неизвестный ключ не должен ставить cookie")
	}
}

// TestSetFlashUnknownKeyIsLoud: ключ приходит в setFlash из кода, литералом.
// Тихий возврат при отсутствии в списке защищает не от клиента (для этого есть
// проверка в parseFlash на пути чтения cookie), а прячет ошибку программиста:
// забытый ключ даёт молчание вместо сообщения, и администратор не отличает
// «отозвано» от «форма не сработала» — ровно так и жила находка про отзыв
// приглашения.
func TestSetFlashUnknownKeyIsLoud(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	(&Handler{}).flashOK(rec, "flash.definitely_not_in_the_list", 0)

	if buf.Len() == 0 {
		t.Fatal("неизвестный ключ прошёл молча: забытый в списке ключ никак " +
			"не отличить от несработавшей формы")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("неизвестный ключ всё-таки уехал в cookie")
	}
}
