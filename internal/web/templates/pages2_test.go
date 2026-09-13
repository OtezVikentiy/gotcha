package templates

import (
	"context"
	"regexp"
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

func TestAuthPages(t *testing.T) {
	providers := []OAuthButton{{Name: "yandex", Label: "Войти через Яндекс"}, {Name: "github", Label: "GitHub"}}
	login := renderTo(t, Login("неверный пароль", "", "a@b.c", providers))
	if !strings.Contains(login, "неверный пароль") || !strings.Contains(login, "Яндекс") {
		t.Error("логин должен показать ошибку и OAuth-кнопки")
	}
	if !strings.Contains(login, `value="a@b.c"`) {
		t.Error("логин должен вернуть введённый email в поле формы")
	}
	// экранирование value: "><script> не должен разорвать атрибут и открыть инъекцию — тег остаётся текстом.
	loginXSS := renderTo(t, Login("неверный пароль", "", `"><script>x</script>`, providers))
	if strings.Contains(loginXSS, "<script>") {
		t.Error("email в форме логина должен быть экранирован, а не вставлен как HTML")
	}
	reg := renderTo(t, RegisterForm("", false, "", "", providers))
	if !strings.Contains(reg, "GitHub") {
		t.Error("регистрация должна показать OAuth-кнопки")
	}
	// ветки закрытой регистрации мутационно проверяет TestRegisterClosed — здесь только заголовок,
	// чтобы не вернуть тавтологичный len(out)==0, не ловящий перепутанные ветки closed/invite.
	regClosed := renderTo(t, RegisterStub("", "closed", "", nil))
	wantClosedTitle := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "auth.register.closed_title")
	if !strings.Contains(regClosed, wantClosedTitle) {
		t.Errorf("закрытая регистрация: нет заголовка закрытой регистрации %q", wantClosedTitle)
	}
	sso := renderTo(t, SSOLogin("домен не настроен"))
	if !strings.Contains(sso, "домен не настроен") {
		t.Error("SSO-логин должен показать ошибку")
	}
}

func TestConfirmPage(t *testing.T) {
	hidden := []HiddenField{{Name: "id", Value: "42"}, {Name: "csrf", Value: "tok"}}
	out := renderTo(t, ConfirmPage("Удалить проект?", "Действие необратимо", "Удалить", "/back", "/do-delete", hidden, "u@e.com"))
	if !strings.Contains(out, "Удалить проект?") || !strings.Contains(out, `value="42"`) {
		t.Error("подтверждение должно показать заголовок и скрытые поля")
	}
}

func TestErrorPage(t *testing.T) {
	out := renderTo(t, ErrorPage(404, "не найдено", "u@e.com"))
	if len(out) == 0 {
		t.Error("страница 404 должна рендериться")
	}
	out2 := renderTo(t, ErrorPage(418, "я чайник", "u@e.com"))
	if !strings.Contains(out2, "я чайник") {
		t.Error("неизвестный статус показывает переданное сообщение")
	}
	// без nav.Shell в ctx страница не должна угадывать проект — выход только «На главную».
	if strings.Contains(out, "/issues") {
		t.Errorf("без shell в ctx страница ошибки не должна угадывать проект: %s", out)
	}
}

// с shell в ctx — второй выход в «Проблемы» текущего проекта (effectiveProjectID: ProjectID, иначе первый).
func TestErrorPageIssuesExit(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	ctx = nav.WithShell(ctx, nav.Shell{
		Projects:  []nav.Project{{ID: 7, Slug: "web", Name: "Веб"}},
		ProjectID: 7,
	})
	var sb strings.Builder
	if err := ErrorPage(404, "", "u@e.com").Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, `href="/projects/7/issues"`) {
		t.Errorf("страница ошибки при известном проекте обязана вести в его «Проблемы»: %s", out)
	}
	if !strings.Contains(out, "К проблемам проекта") {
		t.Errorf("ссылка в «Проблемы» без подписи error.issues_link: %s", out)
	}
	if !strings.Contains(out, `href="/"`) {
		t.Errorf("«На главную» обязана остаться: %s", out)
	}
}

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

func TestMaintenance(t *testing.T) {
	now := time.Now()
	windows := []uptime.Window{
		{ID: 1, Name: "разовое", Weekly: false, StartsAt: ptrTime(now), EndsAt: ptrTime(now.Add(time.Hour)), Timezone: "UTC"},
		{ID: 2, Name: "еженедельное", Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "Europe/Moscow"},
	}
	out := renderTo(t, Maintenance(7, windows, nil, "", "u@e.com"))
	if !strings.Contains(out, "разовое") || !strings.Contains(out, "еженедельное") {
		t.Error("окна обслуживания должны отрендериться")
	}
	outErr := renderTo(t, Maintenance(7, nil, nil, "плохое время", "u@e.com"))
	if !strings.Contains(outErr, "плохое время") {
		t.Error("ошибка обслуживания должна отрендериться")
	}
}

func TestProjectSetup(t *testing.T) {
	project := org.Project{ID: 7, Slug: "web", Name: "Web", Platform: "go"}
	snippets := []SetupSnippet{
		{Lang: "Go", Install: "go get github.com/getsentry/sentry-go", Code: "sentry.Init(...)"},
		{Lang: "PHP", Install: "composer require sentry/sentry", Code: "\\Sentry\\init(...)"},
	}
	out := renderTo(t, ProjectSetup(project, "https://key@dsn/7", snippets, "", "u@e.com"))
	if !strings.Contains(out, "https://key@dsn/7") {
		t.Error("экран установки должен показать DSN")
	}
	// без команды установки сниппет инициализации бесполезен.
	if !strings.Contains(out, "go get github.com/getsentry/sentry-go") {
		t.Error("экран установки должен показать команду установки пакета")
	}
	if !strings.Contains(out, "composer require sentry/sentry") {
		t.Error("экран установки должен показать все переданные сниппеты")
	}
	// сниппеты используют разные DSN (browser у JavaScript, server у остальных) — страница должна это объяснить.
	if !strings.Contains(out, "DSN браузерного ключа") {
		t.Error("экран установки должен объяснить разницу browser-/server-DSN у сниппетов")
	}
	if !strings.Contains(out, `href="/projects/7/issues"`) {
		t.Errorf("экран установки без ссылки в «Проблемы» проекта: %s", out)
	}
	empty := renderTo(t, ProjectSetup(project, "", nil, "", "u@e.com"))
	if strings.Contains(empty, "DSN браузерного ключа") {
		t.Error("без сниппетов пояснение про виды DSN показывать незачем")
	}
}

func TestInviteAccept(t *testing.T) {
	inv := org.InviteInfo{OrgID: 1, OrgName: "Acme Inc", Email: "u@e.com", Role: org.RoleMember}
	out := renderTo(t, InviteAccept("invtok", "", "u@e.com", inv))
	if !strings.Contains(out, "invtok") {
		t.Error("экран приглашения должен нести токен")
	}
	outErr := renderTo(t, InviteAccept("invtok", "просрочено", "u@e.com", org.InviteInfo{}))
	if !strings.Contains(outErr, "просрочено") {
		t.Error("ошибка приглашения должна отрендериться")
	}
}

func TestProfileFlame(t *testing.T) {
	vm := ProfileFlameVM{ProjectID: 7, Service: "web", Type: "cpu", Transaction: "GET /", Environment: "production", Range: TimeRangeVM{Key: "24h"}, Chart: stub()}
	out := renderTo(t, ProfileFlame(vm, "u@e.com"))
	if !strings.Contains(out, "web") {
		t.Error("флейм профиля должен содержать сервис")
	}
	if !strings.Contains(out, "web · cpu") || !strings.Contains(out, "· GET /") {
		t.Errorf("при заполненных type/transaction разделители обязаны быть: %s", out)
	}
	// без параметров — «(unknown)» без висящего разделителя.
	bare := renderTo(t, ProfileFlame(ProfileFlameVM{ProjectID: 7, Range: TimeRangeVM{Key: "24h"}, Chart: stub()}, "u@e.com"))
	if !strings.Contains(bare, "(unknown)") {
		t.Errorf("пустой сервис показывается как (unknown): %s", bare)
	}
	if strings.Contains(bare, "(unknown) ·") || regexp.MustCompile(`\(unknown\)\s*·`).MatchString(bare) {
		t.Errorf("пустые type/transaction не должны оставлять висящий разделитель после (unknown): %s", bare)
	}
}

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

// число дней — из RetentionDays, не захардкожено; склоняется по CLDR ru («1 день»/«2 дня»/«5 дней»).
// RetentionDays<=0 — TTL не задан (хранится вечно): текст без чисел.
func TestTraceExpired(t *testing.T) {
	cases := []struct {
		days int
		want string
	}{
		{days: 1, want: "хранятся 1 день"},
		{days: 2, want: "хранятся 2 дня"},
		{days: 5, want: "хранятся 5 дней"},
		{days: 30, want: "хранятся 30 дней"},
	}
	for _, c := range cases {
		d := TraceExpiredData{ProjectID: 7, TraceID: "trace-exp", RetentionDays: c.days}
		out := renderTo(t, TraceExpired(d, "u@e.com"))
		if !strings.Contains(out, "trace-exp") {
			t.Errorf("RetentionDays=%d: должен содержать trace_id", c.days)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("RetentionDays=%d: missing %q: %s", c.days, c.want, out)
		}
	}

	// RetentionDays=0 — TTL не задан: текст не должен называть срок («хранятся 0 дней» была бы неправдой).
	purged := renderTo(t, TraceExpired(TraceExpiredData{ProjectID: 7, TraceID: "trace-purged", RetentionDays: 0}, "u@e.com"))
	if strings.Contains(purged, "хранятся") {
		t.Errorf("RetentionDays=0 не должен утверждать срок хранения: %s", purged)
	}
	if !strings.Contains(purged, "были удалены") {
		t.Errorf("RetentionDays=0 должен объяснять пропажу спанов без TTL: %s", purged)
	}
}

func TestStatusPagesSettings(t *testing.T) {
	forms := []StatusPageForm{
		{ID: 1, PublicID: "p_public123", Title: "Статус Acme", Description: "Наш статус", Enabled: true, Monitors: []StatusPageFormMonitor{
			{ID: 10, MonitorName: "web", Selected: true, DisplayName: "Веб"},
			{ID: 20, MonitorName: "api", Selected: false},
		}},
	}
	newForm := StatusPageForm{Monitors: []StatusPageFormMonitor{{ID: 10, MonitorName: "web"}}}
	out := renderTo(t, StatusPagesSettings(7, "https://gotcha.example", forms, newForm, true, "", "u@e.com"))
	if !strings.Contains(out, "Статус Acme") || !strings.Contains(out, "p_public123") {
		t.Error("настройки статус-страниц должны содержать форму и публичный адрес")
	}
	outErr := renderTo(t, StatusPagesSettings(7, "https://x", nil, newForm, true, "ошибка формы", "u@e.com"))
	if !strings.Contains(outErr, "ошибка формы") {
		t.Error("ошибка статус-страницы должна отрендериться")
	}
	// canManage=false скрывает enabled; slug нет в форме ни у кого.
	outOperator := renderTo(t, StatusPagesSettings(7, "https://gotcha.example", forms, newForm, false, "", "u@e.com"))
	if strings.Contains(outOperator, `name="enabled"`) {
		t.Error("оператор без прав управления не должен видеть чекбокс enabled")
	}
	if strings.Contains(outOperator, `name="slug"`) {
		t.Error("формы больше не должны присылать slug")
	}
	if strings.Contains(outOperator, "status-page-delete-form") {
		t.Error("оператор не должен видеть кнопку удаления опубликованной статус-страницы")
	}
	if !strings.Contains(out, "status-page-delete-form") {
		t.Error("управляющий должен видеть кнопку удаления статус-страницы")
	}
	draft := []StatusPageForm{{ID: 2, Title: "Черновик", Enabled: false}}
	outOperatorDraft := renderTo(t, StatusPagesSettings(7, "https://gotcha.example", draft, newForm, false, "", "u@e.com"))
	if !strings.Contains(outOperatorDraft, "status-page-delete-form") {
		t.Error("оператор должен видеть кнопку удаления НЕопубликованной статус-страницы")
	}
}

func TestPublicStatusPage(t *testing.T) {
	for _, overall := range []string{"operational", "partial", "major"} {
		v := StatusPageView{
			Title: "Acme Status", Description: "Текущий статус сервисов", Overall: overall,
			Monitors: []StatusMonitorView{
				{Name: "web", Status: "up", Uptime90d: uptime.UptimeStat{Total: 1000, OK: 999}, Bars: stub()},
				{Name: "api", Status: "down", Uptime90d: uptime.UptimeStat{Total: 1000, OK: 800}, Bars: stub()},
				{Name: "cdn", Status: "maintenance", Bars: stub()},
			},
			Incidents:   []StatusIncidentView{{Name: "Сбой API", StartedAt: "2026-07-20 10:00", Ongoing: true}, {Name: "Прошлый сбой", StartedAt: "2026-07-19 08:00", Duration: 2 * time.Hour, Ongoing: false}},
			Maintenance: []StatusWindowView{{Name: "Плановые работы", From: "2026-07-22 02:00", To: "2026-07-22 04:00"}},
		}
		out := renderTo(t, PublicStatusPage(v))
		if !strings.Contains(out, "Acme Status") || !strings.Contains(out, "web") {
			t.Errorf("публичная статус-страница (overall=%s) должна содержать заголовок и мониторы", overall)
		}
	}
}

// длинное имя монитора (FQDN) распирало страницу без scrollRegion — таблицы были без своего скролла.
// ассерт целится в структуру div.table-scroll (tabindex/role/aria-label), не в len(out)!=0.
func TestPublicStatusPageTablesScrollable(t *testing.T) {
	v := StatusPageView{
		Title: "Acme Status", Overall: "operational",
		Incidents:   []StatusIncidentView{{Name: "Сбой API", StartedAt: "2026-07-20 10:00", Ongoing: true}},
		Maintenance: []StatusWindowView{{Name: "Плановые работы", From: "2026-07-22 02:00", To: "2026-07-22 04:00"}},
	}
	out := renderTo(t, PublicStatusPage(v))

	// обёртка инцидентов — свой aria-label, отличный от label окон обслуживания.
	if !strings.Contains(out, `<div class="table-scroll" tabindex="0" role="region" aria-label="Таблица инцидентов">`) {
		t.Error("таблица инцидентов должна быть обёрнута scrollRegion (table-scroll/tabindex/role/aria-label)")
	}
	if !strings.Contains(out, `<div class="table-scroll" tabindex="0" role="region" aria-label="Таблица окон обслуживания">`) {
		t.Error("таблица окон обслуживания должна быть обёрнута scrollRegion (table-scroll/tabindex/role/aria-label)")
	}
	tail := func(i int) string {
		end := i + 400
		if end > len(out) {
			end = len(out)
		}
		return out[i:end]
	}
	if i := strings.Index(out, `aria-label="Таблица инцидентов">`); i < 0 || !strings.Contains(tail(i), `class="status-incidents data-table"`) {
		t.Error("таблица status-incidents должна рендериться внутри своего scrollRegion")
	}
	if i := strings.Index(out, `aria-label="Таблица окон обслуживания">`); i < 0 || !strings.Contains(tail(i), `class="status-maintenance data-table"`) {
		t.Error("таблица status-maintenance должна рендериться внутри своего scrollRegion")
	}
}

// имя сжимается многоточием (CSS) — полное должно остаться доступным через title.
func TestStatusTileNameCarriesFullNameInTitle(t *testing.T) {
	long := "payments-gateway-eu-central-1.internal.example.com"
	v := StatusPageView{
		Title: "Acme Status", Overall: "operational",
		Monitors: []StatusMonitorView{{Name: long, Status: "up", Bars: stub()}},
	}
	out := renderTo(t, PublicStatusPage(v))
	want := `<span class="status-tile-name" title="` + long + `">` + long + `</span>`
	if !strings.Contains(out, want) {
		t.Errorf("status-tile-name должен нести полное имя монитора в title, не нашёл %q в выводе", want)
	}
}

// цель — конкретная форма (2*time.Hour), не «ru≠en» в целом: то было зелёным ещё до правки бага.
// кеш вьюхи общий на всех языках — вьюха несёт time.Duration, перевод происходит при рендере.
func TestStatusPageIncidentDurationLocalised(t *testing.T) {
	v := StatusPageView{
		Title: "Acme Status", Description: "d", Overall: "operational",
		Incidents: []StatusIncidentView{{Name: "API", StartedAt: "2026-07-20 10:00", Duration: 2 * time.Hour}},
	}
	ruCtx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	enCtx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})

	var ruBuf, enBuf strings.Builder
	if err := PublicStatusPage(v).Render(ruCtx, &ruBuf); err != nil {
		t.Fatalf("render ru: %v", err)
	}
	if err := PublicStatusPage(v).Render(enCtx, &enBuf); err != nil {
		t.Fatalf("render en: %v", err)
	}
	ru, en := ruBuf.String(), enBuf.String()

	const ruWant, enWant = "2 часа", "2 hours"
	if !strings.Contains(ru, ruWant) {
		t.Errorf("ru-рендер не содержит %q (длительность не локализована): %s", ruWant, ru)
	}
	if strings.Contains(ru, enWant) {
		t.Errorf("ru-рендер содержит английскую форму %q", enWant)
	}
	if !strings.Contains(en, enWant) {
		t.Errorf("en-рендер не содержит %q (длительность не локализована): %s", enWant, en)
	}
	if strings.Contains(en, ruWant) {
		t.Errorf("en-рендер содержит русскую форму %q", ruWant)
	}
}

func TestHeartbeatMonitorDetail(t *testing.T) {
	m := uptime.Monitor{ID: 4, Name: "cron", Kind: uptime.KindHeartbeat, Enabled: false, IntervalSeconds: 3600, HeartbeatToken: "hbtok"}
	stat := uptime.UptimeStat{Total: 10, OK: 10}
	out := renderTo(t, MonitorDetail(m, "up", stat, stat, stat, stub(), TimeRangeVM{Key: "24h"}, nil, nil, 1, 0, true, true, "https://gotcha.example", "u@e.com", false))
	if !strings.Contains(out, "hbtok") {
		t.Error("деталь heartbeat должна содержать токен пинга")
	}
	// команда — явный -X POST, не голый curl (GET неотличим от префетч-бота/антивирусного прокси).
	if !strings.Contains(out, "curl -fsS -X POST ") {
		t.Error("cron-сниппет heartbeat должен содержать \"curl -fsS -X POST \"")
	}
	// «скопируйте URL сейчас» — URL и cron идут через @copyBlock с кнопкой, не голым <code>.
	for _, id := range []string{"heartbeat-ping-url", "heartbeat-cron-line"} {
		if !strings.Contains(out, `data-copy-target="`+id+`"`) {
			t.Errorf("heartbeat-блок без кнопки копирования %q: %s", id, out)
		}
	}
	if strings.Contains(out, "<code>https://gotcha.example/uptime/hb/hbtok") {
		t.Error("URL пинга по-прежнему голым <code> вместо copyBlock")
	}
}

func TestLayoutShellRendersRail(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	ctx = theme.WithTheme(ctx, theme.Theme{Code: "dark"})
	ctx = nav.WithShell(ctx, nav.Shell{
		UserEmail: "u@e.com",
		Projects:  []nav.Project{{ID: 7, Slug: "web", Name: "Веб-проект"}, {ID: 8, Slug: "api", Name: "API"}},
		ProjectID: 7, OrgID: 1, Area: "issues", Path: "/p/7/issues", CanManage: true,
	})
	var sb strings.Builder
	if err := IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, false, false, false).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Веб-проект") {
		t.Error("сайдбар должен показать имя текущего проекта")
	}
}

func TestLayoutHelpersDirect(t *testing.T) {
	if railAreaClass(true) == railAreaClass(false) {
		t.Error("активный/неактивный rail должны отличаться классом")
	}
	if ctxItemClass(true) == ctxItemClass(false) {
		t.Error("активный/неактивный пункт должны отличаться классом")
	}
	if currentProjectName(nav.Shell{}) != "" || effectiveProjectID(nav.Shell{}) != 0 {
		t.Error("пустой Shell должен давать нулевые значения")
	}
	s := nav.Shell{Projects: []nav.Project{{ID: 9, Name: "first"}}}
	if currentProjectName(s) != "first" || effectiveProjectID(s) != 9 {
		t.Error("без ProjectID берётся первый проект")
	}
	if itoa(-5) != "-5" {
		t.Error("itoa сломан")
	}
}
