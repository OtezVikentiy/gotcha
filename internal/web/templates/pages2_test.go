package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/nav"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/theme"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestAuthPages — экраны входа/регистрации/SSO с OAuth-кнопками и ошибками;
// регистрация в закрытом режиме прячет форму.
func TestAuthPages(t *testing.T) {
	providers := []OAuthButton{{Name: "yandex", Label: "Войти через Яндекс"}, {Name: "github", Label: "GitHub"}}
	login := renderTo(t, Login("неверный пароль", providers))
	if !strings.Contains(login, "неверный пароль") || !strings.Contains(login, "Яндекс") {
		t.Error("логин должен показать ошибку и OAuth-кнопки")
	}
	reg := renderTo(t, Register("", false, providers))
	if !strings.Contains(reg, "GitHub") {
		t.Error("регистрация должна показать OAuth-кнопки")
	}
	regClosed := renderTo(t, Register("", true, nil))
	if len(regClosed) == 0 {
		t.Error("закрытая регистрация всё равно рендерится")
	}
	sso := renderTo(t, SSOLogin("домен не настроен"))
	if !strings.Contains(sso, "домен не настроен") {
		t.Error("SSO-логин должен показать ошибку")
	}
}

// TestConfirmPage — страница подтверждения с действием и скрытыми полями.
func TestConfirmPage(t *testing.T) {
	hidden := []HiddenField{{Name: "id", Value: "42"}, {Name: "csrf", Value: "tok"}}
	out := renderTo(t, ConfirmPage("Удалить проект?", "Действие необратимо", "Удалить", "/back", "/do-delete", hidden, "u@e.com"))
	if !strings.Contains(out, "Удалить проект?") || !strings.Contains(out, `value="42"`) {
		t.Error("подтверждение должно показать заголовок и скрытые поля")
	}
}

// TestErrorPage — страница ошибки для известного и неизвестного статуса.
func TestErrorPage(t *testing.T) {
	out := renderTo(t, ErrorPage(404, "не найдено", "u@e.com"))
	if len(out) == 0 {
		t.Error("страница 404 должна рендериться")
	}
	// Неизвестный статус (ветка с пустыми ключами) — использует msg.
	out2 := renderTo(t, ErrorPage(418, "я чайник", "u@e.com"))
	if !strings.Contains(out2, "я чайник") {
		t.Error("неизвестный статус показывает переданное сообщение")
	}
}

// TestDocsPages — индекс документации по группам и страница с телом.
func TestDocsPages(t *testing.T) {
	groups := []DocsGroup{
		{Key: "docs.group.getting_started", Pages: []docs.Page{{Slug: "quickstart", Group: "docs.group.getting_started", Title: "Быстрый старт"}}},
	}
	idx := renderTo(t, DocsIndex(groups, "u@e.com"))
	if !strings.Contains(idx, "Быстрый старт") {
		t.Error("индекс документации должен содержать заголовок страницы")
	}
	pages := []docs.Page{{Slug: "quickstart", Title: "Быстрый старт"}, {Slug: "sdk", Title: "SDK"}}
	page := renderTo(t, DocsPage("quickstart", "Быстрый старт", "<p>тело статьи</p>", pages, "u@e.com"))
	if !strings.Contains(page, "тело статьи") {
		t.Error("страница документации должна содержать сырое тело статьи")
	}
}

// TestMaintenance — окна обслуживания: разовое (по датам) и еженедельное (по
// дню недели и времени).
func TestMaintenance(t *testing.T) {
	now := time.Now()
	windows := []uptime.Window{
		{ID: 1, Name: "разовое", Weekly: false, StartsAt: ptrTime(now), EndsAt: ptrTime(now.Add(time.Hour)), Timezone: "UTC"},
		{ID: 2, Name: "еженедельное", Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "Europe/Moscow"},
	}
	out := renderTo(t, Maintenance(7, windows, "", "u@e.com"))
	if !strings.Contains(out, "разовое") || !strings.Contains(out, "еженедельное") {
		t.Error("окна обслуживания должны отрендериться")
	}
	// С ошибкой и пустым списком.
	outErr := renderTo(t, Maintenance(7, nil, "плохое время", "u@e.com"))
	if !strings.Contains(outErr, "плохое время") {
		t.Error("ошибка обслуживания должна отрендериться")
	}
}

// TestProjectSetup — экран инструкций по установке SDK с DSN и сниппетами.
func TestProjectSetup(t *testing.T) {
	project := org.Project{ID: 7, Slug: "web", Name: "Web", Platform: "go"}
	out := renderTo(t, ProjectSetup(project, "https://key@dsn/7", "go-snippet", "php-snippet", "js-snippet", "u@e.com"))
	if !strings.Contains(out, "https://key@dsn/7") {
		t.Error("экран установки должен показать DSN")
	}
}

// TestInviteAccept — экран принятия приглашения с токеном и ошибкой.
func TestInviteAccept(t *testing.T) {
	out := renderTo(t, InviteAccept("invtok", "", "u@e.com"))
	if !strings.Contains(out, "invtok") {
		t.Error("экран приглашения должен нести токен")
	}
	outErr := renderTo(t, InviteAccept("invtok", "просрочено", "u@e.com"))
	if !strings.Contains(outErr, "просрочено") {
		t.Error("ошибка приглашения должна отрендериться")
	}
}

// TestProfileFlame — страница флеймграфа профиля с графиком.
func TestProfileFlame(t *testing.T) {
	vm := ProfileFlameVM{ProjectID: 7, Service: "web", Type: "cpu", Transaction: "GET /", Environment: "production", Period: "24h", Chart: stub()}
	out := renderTo(t, ProfileFlame(vm, "u@e.com"))
	if !strings.Contains(out, "web") {
		t.Error("флейм профиля должен содержать сервис")
	}
}

// TestTracePages — waterfall и флеймграф трейса.
func TestTracePages(t *testing.T) {
	wf := TraceWaterfallData{ProjectID: 7, TraceID: "trace123", Transaction: "GET /api", TotalUS: 250000, Timestamp: time.Now(), Waterfall: stub(), ShownRows: 5, TotalRows: 10, HasProfile: true, From: "endpoint", FromTransaction: "GET /api"}
	out := renderTo(t, TraceWaterfall(wf, "u@e.com"))
	if !strings.Contains(out, "trace123") {
		t.Error("waterfall должен содержать trace id")
	}
	fl := TraceFlameData{TraceID: "trace123", Chart: stub()}
	outF := renderTo(t, TraceFlame(fl, "u@e.com"))
	if !strings.Contains(outF, "trace123") {
		t.Error("флейм трейса должен содержать trace id")
	}
}

// TestStatusPagesSettings — настройки статус-страниц: существующая форма с
// мониторами и новая пустая форма.
func TestStatusPagesSettings(t *testing.T) {
	forms := []StatusPageForm{
		{ID: 1, Slug: "public", Title: "Статус Acme", Description: "Наш статус", Enabled: true, Monitors: []StatusPageFormMonitor{
			{ID: 10, MonitorName: "web", Selected: true, DisplayName: "Веб"},
			{ID: 20, MonitorName: "api", Selected: false},
		}},
	}
	newForm := StatusPageForm{Monitors: []StatusPageFormMonitor{{ID: 10, MonitorName: "web"}}}
	out := renderTo(t, StatusPagesSettings(7, "https://gotcha.example", forms, newForm, "", "u@e.com"))
	if !strings.Contains(out, "Статус Acme") || !strings.Contains(out, "public") {
		t.Error("настройки статус-страниц должны содержать форму")
	}
	// С ошибкой.
	outErr := renderTo(t, StatusPagesSettings(7, "https://x", nil, newForm, "занятый slug", "u@e.com"))
	if !strings.Contains(outErr, "занятый slug") {
		t.Error("ошибка статус-страницы должна отрендериться")
	}
}

// TestPublicStatusPage — публичная статус-страница со всеми сводными статусами,
// мониторами, инцидентами и окнами обслуживания.
func TestPublicStatusPage(t *testing.T) {
	for _, overall := range []string{"operational", "partial", "major"} {
		v := StatusPageView{
			Title: "Acme Status", Description: "Текущий статус сервисов", Overall: overall,
			Monitors: []StatusMonitorView{
				{Name: "web", Status: "up", Uptime90d: uptime.UptimeStat{Total: 1000, OK: 999}, Bars: stub()},
				{Name: "api", Status: "down", Uptime90d: uptime.UptimeStat{Total: 1000, OK: 800}, Bars: stub()},
				{Name: "cdn", Status: "maintenance", Bars: stub()},
			},
			Incidents:   []StatusIncidentView{{Name: "Сбой API", StartedAt: "2026-07-20 10:00", Ongoing: true}, {Name: "Прошлый сбой", StartedAt: "2026-07-19 08:00", Duration: "2h", Ongoing: false}},
			Maintenance: []StatusWindowView{{Name: "Плановые работы", From: "2026-07-22 02:00", To: "2026-07-22 04:00"}},
		}
		out := renderTo(t, PublicStatusPage(v))
		if !strings.Contains(out, "Acme Status") || !strings.Contains(out, "web") {
			t.Errorf("публичная статус-страница (overall=%s) должна содержать заголовок и мониторы", overall)
		}
	}
}

// TestHeartbeatMonitorDetail — деталь heartbeat-монитора рисует URL пинга и
// cron-сниппет (heartbeatPingURL/heartbeatCronSnippet).
func TestHeartbeatMonitorDetail(t *testing.T) {
	m := uptime.Monitor{ID: 4, Name: "cron", Kind: uptime.KindHeartbeat, Enabled: false, IntervalSeconds: 3600, HeartbeatToken: "hbtok"}
	stat := uptime.UptimeStat{Total: 10, OK: 10}
	out := renderTo(t, MonitorDetail(m, "up", stat, stat, stat, stub(), nil, nil, true, "https://gotcha.example", "u@e.com"))
	if !strings.Contains(out, "hbtok") {
		t.Error("деталь heartbeat должна содержать токен пинга")
	}
}

// TestLayoutShellRendersRail — рендер страницы внутри nav.Shell раскрывает
// боковую навигацию: имя текущего проекта и ссылки разделов (railAreaClass,
// ctxItemClass, currentProjectName, effectiveProjectID).
func TestLayoutShellRendersRail(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	ctx = theme.WithTheme(ctx, theme.Theme{Code: "dark"})
	ctx = nav.WithShell(ctx, nav.Shell{
		UserEmail: "u@e.com",
		Projects:  []nav.Project{{ID: 7, Slug: "web", Name: "Веб-проект"}, {ID: 8, Slug: "api", Name: "API"}},
		ProjectID: 7, OrgID: 1, Area: "issues", Path: "/p/7/issues", CanManage: true,
	})
	var sb strings.Builder
	if err := IssuesList(7, nil, IssuesFilter{}, 1, 0, true, "u@e.com", nil, nil, GettingStartedVM{}).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Веб-проект") {
		t.Error("сайдбар должен показать имя текущего проекта")
	}
}

// TestLayoutHelpersDirect — прямые проверки хелперов сайдбара на граничных
// входах (пустой Shell → падать не должен).
func TestLayoutHelpersDirect(t *testing.T) {
	if railAreaClass(true) == railAreaClass(false) {
		t.Error("активный/неактивный rail должны отличаться классом")
	}
	if ctxItemClass(true) == ctxItemClass(false) {
		t.Error("активный/неактивный пункт должны отличаться классом")
	}
	// Пустой Shell: имя пусто, id ноль — без паники.
	if currentProjectName(nav.Shell{}) != "" || effectiveProjectID(nav.Shell{}) != 0 {
		t.Error("пустой Shell должен давать нулевые значения")
	}
	// Shell без ProjectID падает на первый проект.
	s := nav.Shell{Projects: []nav.Project{{ID: 9, Name: "first"}}}
	if currentProjectName(s) != "first" || effectiveProjectID(s) != 9 {
		t.Error("без ProjectID берётся первый проект")
	}
	if itoa(-5) != "-5" {
		t.Error("itoa сломан")
	}
}
