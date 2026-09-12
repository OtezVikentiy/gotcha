package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestWebStatusPageCSSUsesVersionedURL(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spcss")
	m := statusPageMonitor(t, s, proj.ID, "css-monitor", "https://example.com/css")

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "CSS Cache", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/"+sp.PublicID)
	if status != http.StatusOK {
		t.Fatalf("GET /status/%s = %d, want 200: %s", sp.PublicID, status, body)
	}
	if !strings.Contains(body, `href="/static/app.css?v=`) {
		t.Fatalf("публичная статус-страница ссылается на app.css без ?v= (голый путь попадает в короткий max-age=3600, а не в immutable вместе с кабинетом): %s", body)
	}
}

// матчим по data-status-note, не по классу — "hint" общий у обеих подсказок.
func extractHint(t *testing.T, html, note string) string {
	t.Helper()
	marker := `data-status-note="` + note + `"`
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("блок %s не найден на странице: %s", note, html)
	}
	rest := html[i:]
	end := strings.Index(rest, "</p>")
	if end < 0 {
		t.Fatalf("блок %s не закрыт тегом </p>: %s", note, html)
	}
	return rest[:end]
}

// ловит тихий фолбэк i18n.T на RU при отсутствии ключа в EN — сравнение по
// общему для языков слову вроде "UTC" такой фолбэк не заметит.
func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 0x0400 && r <= 0x04FF {
			return true
		}
	}
	return false
}

func TestWebStatusPagePublicSurfaceExplainsItself(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spexplain")
	m := statusPageMonitor(t, s, proj.ID, "explain-monitor", "https://example.com/explain")
	// блок data-status-note="paused" рендерится только при мониторе на паузе.
	if err := s.uptime.SetEnabled(context.Background(), m.ID, false); err != nil {
		t.Fatalf("pause monitor: %v", err)
	}

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Explain", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	url := s.srv.URL + "/status/" + sp.PublicID

	ru := getAnonLang(t, url, "ru-RU,ru;q=0.9")
	ruTZ := extractHint(t, ru, "timezone")
	if !strings.Contains(ruTZ, "показаны по UTC") {
		t.Errorf("русская страница не объясняет часовой пояс по-русски: %s", ruTZ)
	}
	ruPaused := extractHint(t, ru, "paused")
	if !strings.Contains(ruPaused, "Пауза") || !strings.Contains(ruPaused, "не сбой") {
		t.Errorf("русская страница не расшифровывает статус «Пауза»: %s", ruPaused)
	}

	en := getAnonLang(t, url, "en-US,en;q=0.9")
	enTZ := extractHint(t, en, "timezone")
	if !strings.Contains(enTZ, "shown in UTC") {
		t.Errorf("английская страница не объясняет часовой пояс по-английски: %s", enTZ)
	}
	if hasCyrillic(enTZ) {
		t.Errorf("английская подсказка про часовой пояс содержит кириллицу — похоже на фолбэк i18n.T на RU: %s", enTZ)
	}
	enPaused := extractHint(t, en, "paused")
	if !strings.Contains(enPaused, "Paused") || !strings.Contains(enPaused, "outage") {
		t.Errorf("английская страница не расшифровывает статус Paused: %s", enPaused)
	}
	if hasCyrillic(enPaused) {
		t.Errorf("английская подсказка про Paused содержит кириллицу — похоже на фолбэк i18n.T на RU: %s", enPaused)
	}
}

func TestWebStatusPagePausedHintOnlyWhenPaused(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "sppausehint")

	none := statusPageMonitor(t, s, proj.ID, "no-pause-monitor", "https://example.com/no-pause")
	spNone, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "NoPause", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: none.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page (no pause): %v", err)
	}
	_, bodyNone := getAnon(t, s.srv, "/status/"+spNone.PublicID)
	if strings.Contains(bodyNone, `data-status-note="paused"`) {
		t.Errorf("подсказка про «Паузу» показана на странице, где ни один монитор не на паузе: %s", bodyNone)
	}

	paused := statusPageMonitor(t, s, proj.ID, "pause-monitor", "https://example.com/pause")
	if err := s.uptime.SetEnabled(context.Background(), paused.ID, false); err != nil {
		t.Fatalf("pause monitor: %v", err)
	}
	spPaused, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Pause", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: paused.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page (pause): %v", err)
	}
	_, bodyPaused := getAnon(t, s.srv, "/status/"+spPaused.PublicID)
	if !strings.Contains(bodyPaused, `data-status-note="paused"`) {
		t.Errorf("подсказка про «Паузу» не показана на странице с монитором на паузе: %s", bodyPaused)
	}
}
