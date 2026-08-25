package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
)

// Этот файл защищает пользовательские тексты D3 (лента инцидентов, подавление
// шторма, история SLO) от тихого расхождения с реальным поведением кода —
// ревью R7 нашло, что ни один тест не падал ни при подмене feed.section.closed,
// ни при неточной формулировке alert_suppression.intro.scope. В отличие от
// пары `strings.Contains(out, i18n.T(ctx, key))`, которая самотавтологична
// (обе стороны читают один и тот же JSON и мутация ключа не ловится), здесь
// ожидаемые фразы зашиты литералом — подмена значения ключа в locales/*.json
// обязана уронить соответствующий ассерт.

// TestIncidentFeedSectionHeadingsAreLiteral — заголовки трёх секций ленты
// (открытые группы / вне групп / недавно решённые) и подпись окна ленты
// закрытых — литералами, а не через i18n.T, иначе подмена значения ключа в
// JSON не ловится тестом (см. регрессию feed.section.closed из ревью R7).
func TestIncidentFeedSectionHeadingsAreLiteral(t *testing.T) {
	// Значения — те же, что реально уходят из incidentfeed.go в проде
	// (incidentgroup.MaxOpenGroups/MaxOpenOutOfGroup, feedClosedGroupsLimit/
	// feedClosedOutOfGroupLimit — все 50 сегодня, R4 W7/W8): нулевой FeedCaps{}
	// напечатал бы "не больше 0", это не защитило бы от регрессии текста на
	// реалистичных числах.
	caps := FeedCaps{OpenGroups: 50, OutOfGroup: 50, ClosedGroups: 50, ClosedItems: 50}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := IncidentFeed(1, nil, nil, nil, nil, caps, true, "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"Открытые группы",
		"Вне групп",
		"Недавно решённые",
		"не больше 50",
		"за последние сутки: групп не больше 50, отдельных инцидентов не больше 50",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("IncidentFeed не содержит %q", want)
		}
	}

	ctxEN := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	var sbEN strings.Builder
	if err := IncidentFeed(1, nil, nil, nil, nil, caps, true, "u@example.com").Render(ctxEN, &sbEN); err != nil {
		t.Fatalf("Render en: %v", err)
	}
	outEN := sbEN.String()
	for _, want := range []string{
		"Open groups", "Ungrouped", "Recently resolved",
		"up to 50",
		"last 24 hours: groups up to 50, standalone incidents up to 50",
	} {
		if !strings.Contains(outEN, want) {
			t.Errorf("IncidentFeed(en) не содержит %q", want)
		}
	}

	// Смоук на непустой состав: заголовок закрытой группы и метка "решена"
	// рендерятся, а не только заголовки секций.
	resolved := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	group := NewGroupCard(incidentgroup.GroupRow{
		Group:    incidentgroup.Group{ID: 1, RootSource: "host", StartedAt: resolved.Add(-time.Hour), ResolvedAt: &resolved},
		RootName: "web-1",
	}, nil)
	var sb2 strings.Builder
	if err := IncidentFeed(1, nil, nil, []GroupCard{group}, nil, caps, true, "u@example.com").Render(ctx, &sb2); err != nil {
		t.Fatalf("Render closed group: %v", err)
	}
	if !strings.Contains(sb2.String(), "решена") {
		t.Errorf("IncidentFeed с закрытой группой не содержит метку %q (feed.group.resolved)", "решена")
	}
}

// TestUptimeIncidentsSeeFeedHintIsLiteral — подсказка-ссылка на общую ленту
// со страницы аптайм-инцидентов: обе половины (до и после ссылки) литералом.
func TestUptimeIncidentsSeeFeedHintIsLiteral(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := IncidentsList(1, nil, 1, 0, "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Здесь только инциденты недоступности мониторов") {
		t.Errorf("IncidentsList не содержит начало подсказки uptime.incidents.see_feed_hint")
	}
	if !strings.Contains(out, "».") {
		t.Errorf("IncidentsList не содержит хвост подсказки uptime.incidents.see_feed_hint_suffix")
	}

	ctxEN := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	var sbEN strings.Builder
	if err := IncidentsList(1, nil, 1, 0, "u@example.com").Render(ctxEN, &sbEN); err != nil {
		t.Fatalf("Render en: %v", err)
	}
	outEN := sbEN.String()
	if !strings.Contains(outEN, "This page shows only monitor availability incidents") {
		t.Errorf("IncidentsList(en) не содержит начало подсказки uptime.incidents.see_feed_hint")
	}
}

// TestAlertSuppressionScopeDescribesInformingAndSilentRoots — W26: текст
// подсказки обязан честно описывать ОБА случая гейта D3 (host/incident.go,
// OpenUnacked): под информирующим корнем уведомление ребёнка придерживается
// на всё время, пока группа открыта; под немым корнем ребёнок шлёт первое
// уведомление сам, и только дальнейшая эскалация ждёт закрытия группы. Тест
// обязан упасть, если текст снова схлопнет оба случая в один (как было до
// фикс-раунда R7b).
func TestAlertSuppressionScopeDescribesInformingAndSilentRoots(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := AlertSuppression(1, nil, nil, nil, nil, 0, "", "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"если корень уже разослал своё уведомление",
		"их уведомление придерживается на всё время, пока группа открыта",
		"если корень к этому моменту ещё не уведомлял",
		"первое уведомление ребёнка уходит как обычно",
		"дальнейшая эскалация ждёт закрытия группы",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("AlertSuppression не содержит %q", want)
		}
	}

	ctxEN := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	var sbEN strings.Builder
	if err := AlertSuppression(1, nil, nil, nil, nil, 0, "", "u@example.com").Render(ctxEN, &sbEN); err != nil {
		t.Fatalf("Render en: %v", err)
	}
	outEN := sbEN.String()
	for _, want := range []string{
		"if the root has already sent its own notification",
		"theirs is held back for as long as the group stays open",
		"notified yet",
		"first notification still goes out as usual",
		"further escalation waits until the group closes",
	} {
		if !strings.Contains(outEN, want) {
			t.Errorf("AlertSuppression(en) не содержит %q", want)
		}
	}
}

// TestSLOHistoryResolvedLabelsSayResolvedNotClosed — W31: терминология
// «решён», не «закрыт» — та же метка, что uptime.incident.status_resolved,
// feed.group.resolved и metrics.alerts.status.resolved. Регрессия закрытых
// ru.json-ключей slo.detail.incident_resolved/col_resolved на "Закрыт" не
// ловилась ни одним тестом.
func TestSLOHistoryResolvedLabelsSayResolvedNotClosed(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	started := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	resolvedAt := started.Add(2 * time.Hour)
	vm := SLODetailVM{
		ProjectID: 1, ID: 5, Name: "checkout availability", Kind: "availability",
		TargetPct: 99, WindowDays: 30,
		Chart: templ.NopComponent,
		Incidents: []SLOIncidentRow{
			{Open: false, StartedAt: started, ResolvedAt: &resolvedAt, BurnRate: 16.0},
		},
	}
	var sb strings.Builder
	if err := SLODetailScreen(vm, "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Решён") {
		t.Errorf("SLODetailScreen не содержит метку статуса %q (slo.detail.incident_resolved/col_resolved)", "Решён")
	}
	if strings.Contains(out, "Закрыт") {
		t.Errorf("SLODetailScreen всё ещё содержит устаревшую метку %q вместо «Решён»: %s", "Закрыт", out)
	}
}
