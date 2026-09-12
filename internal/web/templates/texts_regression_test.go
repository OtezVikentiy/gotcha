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

// тексты — литералом, не strings.Contains(out, i18n.T(ctx,key)): тот ассерт самотавтологичен.
// обе стороны читают один JSON — подмена ключа таким ассертом не ловится, литералом — падает.

// openGroups непустые — иначе рендер ушёл бы в ветку «пусто», и заголовки секций не появились бы.
func TestOverviewSectionHeadingsAreLiteral(t *testing.T) {
	// значения как в проде — нулевой FeedCaps{} напечатал бы «не больше 0», не ловя регрессию текста.
	caps := FeedCaps{OpenGroups: 50, OutOfGroup: 50, ClosedGroups: 50, ClosedItems: 50}
	openGroups := []GroupCard{NewGroupCard(
		incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: time.Now()}, RootName: "gw-1"},
		[]incidentgroup.FeedItem{{Source: "host"}},
	)}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := Overview(1, "24h", openGroups, nil, nil, nil, caps, true, StatusLine{}, nil, "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"Открытые группы",
		"Вне групп",
		"Недавно решённые",
		"не больше 50",
		"за последние 24 ч: групп не больше 50, отдельных инцидентов не больше 50",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Overview не содержит %q", want)
		}
	}

	ctxEN := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	var sbEN strings.Builder
	if err := Overview(1, "24h", openGroups, nil, nil, nil, caps, true, StatusLine{}, nil, "u@example.com").Render(ctxEN, &sbEN); err != nil {
		t.Fatalf("Render en: %v", err)
	}
	outEN := sbEN.String()
	for _, want := range []string{
		"Open groups", "Ungrouped", "Recently resolved",
		"up to 50",
		"last 24h: groups up to 50, standalone incidents up to 50",
	} {
		if !strings.Contains(outEN, want) {
			t.Errorf("Overview(en) не содержит %q", want)
		}
	}

	resolved := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	group := NewGroupCard(incidentgroup.GroupRow{
		Group:    incidentgroup.Group{ID: 1, RootSource: "host", StartedAt: resolved.Add(-time.Hour), ResolvedAt: &resolved},
		RootName: "web-1",
	}, nil)
	var sb2 strings.Builder
	if err := Overview(1, "24h", nil, nil, []GroupCard{group}, nil, caps, true, StatusLine{}, nil, "u@example.com").Render(ctx, &sb2); err != nil {
		t.Fatalf("Render closed group: %v", err)
	}
	if !strings.Contains(sb2.String(), "решена") {
		t.Errorf("Overview с закрытой группой не содержит метку %q (feed.group.resolved)", "решена")
	}
}

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

// текст обязан различать оба случая гейта (host/incident.go, OpenUnacked): под информирующим корнем
// уведомление ребёнка ждёт закрытия группы, под немым — первое уходит сразу, ждёт только эскалация.
func TestAlertSuppressionScopeDescribesInformingAndSilentRoots(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := AlertSuppression(1, nil, nil, nil, nil, 0, nil, "", "u@example.com").Render(ctx, &sb); err != nil {
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
	if err := AlertSuppression(1, nil, nil, nil, nil, 0, nil, "", "u@example.com").Render(ctxEN, &sbEN); err != nil {
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

// терминология «решён», не «закрыт» — та же метка, что у uptime/feed/metrics-alerts статусов.
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

// хинт описывает содержимое, не срок — окно «недавно решённые» теперь выбираемое (24ч/7д).
// литералом, не через i18n.T — иначе подмена значения ключа не ловится (см. верх файла).
func TestOverviewSeeIncidentsHintIsLiteral(t *testing.T) {
	caps := FeedCaps{OpenGroups: 50, OutOfGroup: 50, ClosedGroups: 50, ClosedItems: 50}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := Overview(1, "24h", nil, nil, nil, nil, caps, true, StatusLine{}, nil, "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Здесь открытые группы, внегрупповые открытые инциденты и недавно закрытое") {
		t.Errorf("Overview не содержит начало подсказки feed.see_incidents_hint: %s", out)
	}
	if strings.Contains(out, "за последние сутки") {
		t.Errorf("Overview всё ещё зашивает конкретный срок в хинт, хотя окно «недавно решённые» стало выбираемым: %s", out)
	}
	if !strings.Contains(out, "».") {
		t.Errorf("Overview не содержит хвост подсказки feed.see_incidents_hint_suffix: %s", out)
	}

	ctxEN := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	var sbEN strings.Builder
	if err := Overview(1, "24h", nil, nil, nil, nil, caps, true, StatusLine{}, nil, "u@example.com").Render(ctxEN, &sbEN); err != nil {
		t.Fatalf("Render en: %v", err)
	}
	outEN := sbEN.String()
	if !strings.Contains(outEN, "This shows open groups, ungrouped open incidents, and what closed recently") {
		t.Errorf("Overview(en) не содержит начало подсказки feed.see_incidents_hint: %s", outEN)
	}
	if strings.Contains(outEN, "in the last 24 hours") {
		t.Errorf("Overview(en) всё ещё зашивает конкретный срок в хинт, хотя окно «недавно решённые» стало выбираемым: %s", outEN)
	}
}

// потолок нестандартный (17, не 50) — совпадение с дефолтом замаскировало бы регрессию текста.
// openGroups непустые — иначе рендер ушёл бы в ветку «пусто», и секция closed не появилась бы.
func TestOverviewClosedEmptyBodyHasNoStaleCap(t *testing.T) {
	caps := FeedCaps{OpenGroups: 0, OutOfGroup: 0, ClosedGroups: 17, ClosedItems: 17}
	openGroups := []GroupCard{NewGroupCard(
		// время без «50» ни в одном поле — иначе live time.Now() иногда флакует на :50 секунд/минут.
		incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)}, RootName: "gw-1"},
		[]incidentgroup.FeedItem{{Source: "host"}},
	)}
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := Overview(1, "24h", openGroups, nil, nil, nil, caps, true, StatusLine{}, nil, "u@example.com").Render(ctx, &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "групп не больше 17, отдельных инцидентов не больше 17") {
		t.Errorf("Overview не отражает потолок 17 в подписи секции closed: %s", out)
	}
	// фраза целиком, не голое «50» — то совпало бы с чем угодно на странице (id, порт, другая цифра).
	if strings.Contains(out, "не больше 50") {
		t.Errorf("Overview содержит устаревшую фразу потолка «не больше 50» (пустое состояние closed разъехалось с FeedCaps): %s", out)
	}
}
