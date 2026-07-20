package web_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// getAnonLang — анонимный GET с заданным Accept-Language: публичную
// status-страницу смотрит посетитель без сессии, и язык ему может достаться
// только из заголовка (или cookie), не из users.locale.
func getAnonLang(t *testing.T, url, lang string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Language", lang)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (%s) = %d: %s", url, lang, resp.StatusCode, body)
	}
	return string(body)
}

// TestWebStatusPageLocalized — публичная страница переводится по
// Accept-Language, и (главное) язык НЕ протекает через 30-секундный кеш
// вьюхи: сборка кешируется одна на slug, поэтому всё локалезависимое обязано
// строиться на рендере запроса, а не внутри buildStatusPage.
func TestWebStatusPageLocalized(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spi18n")

	api := statusPageMonitor(t, s, proj.ID, "checkout-api-prod", "https://example.com/health")
	at := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := s.uptime.ApplyResult(context.Background(), api.ID, "local", true, "", at); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	s.writer.Add(proj.ID, api.ID, "local", at, uptime.Result{OK: true, StatusCode: 200, TotalMs: 100})
	s.flush(t)

	if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spi18n-status", Title: "Acme Status", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: api.ID, DisplayName: "API", Position: 0}}); err != nil {
		t.Fatalf("create status page: %v", err)
	}

	url := s.srv.URL + "/status/spi18n-status"

	// Первым греет кеш русский посетитель — именно так локаль и протекала бы.
	ru := getAnonLang(t, url, "ru-RU,ru;q=0.9")
	for _, want := range []string{"Все системы работают", "Работает", `lang="ru"`} {
		if !strings.Contains(ru, want) {
			t.Errorf("русская страница без %q", want)
		}
	}

	en := getAnonLang(t, url, "en-US,en;q=0.9")
	for _, want := range []string{"All systems operational", "Operational", `lang="en"`} {
		if !strings.Contains(en, want) {
			t.Errorf("английская страница без %q", want)
		}
	}
	for _, leak := range []string{"Все системы работают", "Работает", "Доступность", "Инциденты за", "работает", "нет данных"} {
		if strings.Contains(en, leak) {
			t.Errorf("русский текст %q протёк в английскую страницу через кеш", leak)
		}
	}
	if strings.Contains(en, "status.") {
		t.Error("в HTML остался сырой ключ каталога — ключа нет в en.json")
	}
}
