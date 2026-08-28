package web_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestWebStatusPageCSSUsesVersionedURL — L1: и основной кабинет
// (layout.templ), и публичная статус-страница ссылаются на один и тот же
// /static/app.css, а web.go проставляет Cache-Control по наличию ?v=,
// совпадающему с текущим хэшем (cacheControl в web.go). Раньше статус-
// страница подключала CSS голым путём и попадала в короткую ветку
// max-age=3600, тогда как тот же URL из кабинета обещал прокси/CDN
// immutable — двойная политика на один URL. Ссылка обязана нести ?v=.
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

// extractHint вырезает содержимое одного из двух блоков-подсказок публичной
// страницы по атрибуту-зацепке data-status-note="timezone"/"paused" — а не по
// классу оформления (обе подсказки визуально используют общий класс "hint",
// он их не различает) и не всей страницы целиком, чтобы проверка на «чужой»
// язык (см. ниже) не спотыкалась о легитимный контент вроде имени монитора,
// заданного пользователем по-русски, которое вполне может стоять на
// английской странице.
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

// hasCyrillic сообщает, есть ли в строке кириллица — дешёвый и надёжный
// сторож против тихого фолбэка i18n.T на локаль по умолчанию (RU), когда в
// запрошенной локали (EN) ключ отсутствует: english.json теряет ключ →
// страница молча рисует русский текст → проверка вида strings.Contains(en,
// "UTC") этого не замечает, потому что "UTC" одинаково в обоих языках.
func hasCyrillic(s string) bool {
	for _, r := range s {
		if r >= 0x0400 && r <= 0x04FF {
			return true
		}
	}
	return false
}

// TestWebStatusPagePublicSurfaceExplainsItself — I2: единственная поверхность
// продукта, которую видит человек без учётной записи, обязана сама объяснять
// себя — в каком часовом поясе показано время и что «Пауза» не значит отказ.
// Проверяется на обеих локалях: подписи — новые ключи i18n, обязаны попасть
// в обе. Ассерты ловят ЛОКАЛИЗОВАННЫЙ текст (часть фразы, которая на RU и EN
// различается), а не общее слово вроде "UTC" — иначе тест слеп к тихому
// фолбэку i18n.T на локаль по умолчанию при отсутствии ключа в запрошенной
// (см. hasCyrillic выше и историю этого теста).
func TestWebStatusPagePublicSurfaceExplainsItself(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spexplain")
	m := statusPageMonitor(t, s, proj.ID, "explain-monitor", "https://example.com/explain")
	// Подсказка про «Паузу» теперь условна (рендерится только когда есть
	// монитор в этом статусе) — монитор страницы обязан быть на паузе,
	// иначе блок data-status-note="paused" не появится вовсе и тест провалится не на
	// той причине.
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

// TestWebStatusPagePausedHintOnlyWhenPaused — I2 P2: подсказка про «Паузу»
// (status.monitor.paused_hint) уместна только там, где посетитель реально
// видит бейдж «Пауза». На странице без единого такого монитора она — шум на
// единственной публичной поверхности продукта.
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
