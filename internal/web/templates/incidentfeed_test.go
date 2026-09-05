package templates

import (
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
)

// TestFeedItemHrefKnownSources — feedItemHref обязан переиспользовать
// билдеры путей родного экрана каждого источника (§6.1): проверяем все 6
// известных веток switch'а по отдельности.
func TestFeedItemHrefKnownSources(t *testing.T) {
	const projectID = int64(42)
	cases := []struct {
		name string
		item incidentgroup.FeedItem
		want string
	}{
		{"host", incidentgroup.FeedItem{Source: "host", RefName: "web-1"}, hostsBasePath(projectID) + "/web-1"},
		{"host escapes ref name", incidentgroup.FeedItem{Source: "host", RefName: "web/1"}, hostsBasePath(projectID) + "/web%2F1"},
		{"uptime", incidentgroup.FeedItem{Source: "uptime", RefID: 7}, monitorDetailPath(7)},
		{"metric", incidentgroup.FeedItem{Source: "metric"}, metricAlertsBasePath(projectID)},
		{"slo", incidentgroup.FeedItem{Source: "slo", RefID: 9}, sloDetailPath(projectID, 9)},
		{"trace", incidentgroup.FeedItem{Source: "trace"}, regressionsPath(projectID)},
		{"profile", incidentgroup.FeedItem{Source: "profile"}, profileRegressionsBasePath(projectID)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedItemHref(projectID, c.item); got != c.want {
				t.Errorf("feedItemHref(%q) = %q, want %q", c.item.Source, got, c.want)
			}
		})
	}
}

// TestFeedItemHrefUnknownSourceFallsBackToOverviewPath — источник, не
// входящий ни в одну из 6 известных веток, обязан упасть в default: ссылка
// на «Обзор» (overviewPath), а не паника/пустая строка.
func TestFeedItemHrefUnknownSourceFallsBackToOverviewPath(t *testing.T) {
	const projectID = int64(42)
	got := feedItemHref(projectID, incidentgroup.FeedItem{Source: "bogus"})
	want := overviewPath(projectID)
	if got != want {
		t.Errorf("feedItemHref(unknown source) = %q, want %q", got, want)
	}
	if want != "/projects/42/overview" {
		t.Errorf("overviewPath(42) = %q, want /projects/42/overview", want)
	}
}

// TestNewGroupCardCounts — счётчики карточки группы считаются по Source
// членов (host N · uptime N · metric N · slo N); неизвестный source (не
// должен встречаться в реальных данных, но defensive) не должен ломать счёт
// и не должен попасть ни в одну из 4 корзин.
func TestNewGroupCardCounts(t *testing.T) {
	members := []incidentgroup.FeedItem{
		{Source: "host"}, {Source: "host"},
		{Source: "uptime"},
		{Source: "metric"}, {Source: "metric"}, {Source: "metric"},
		{Source: "bogus"},
	}
	c := NewGroupCard(incidentgroup.GroupRow{}, members)
	if c.Hosts != 2 || c.Uptime != 1 || c.Metrics != 3 || c.SLOs != 0 {
		t.Fatalf("counts = host=%d uptime=%d metric=%d slo=%d, want 2/1/3/0",
			c.Hosts, c.Uptime, c.Metrics, c.SLOs)
	}
	if len(c.Members) != len(members) {
		t.Fatalf("Members len = %d, want %d (all members kept, including unknown source)", len(c.Members), len(members))
	}
}

// TestGroupCardResolvedBadge — бейдж «решена» рисуется только когда у
// группы проставлен ResolvedAt; счётчики состава попадают в подсказку.
func TestGroupCardResolvedBadge(t *testing.T) {
	base := incidentgroup.GroupRow{
		Group:    incidentgroup.Group{RootSource: "host", StartedAt: time.Now()},
		RootName: "gw-1",
	}
	open := NewGroupCard(base, []incidentgroup.FeedItem{{Source: "host"}, {Source: "uptime"}})
	openHTML := renderTo(t, groupCard(1, open, true))
	if strings.Contains(openHTML, "решена") {
		t.Error("open group card must not show the resolved badge")
	}
	if !strings.Contains(openHTML, "всего 2 (Хост 1 · Аптайм 1)") {
		t.Errorf("open group card missing composition hint: %s", openHTML)
	}
	// Нулевые источники (metric/slo) не должны попадать в подсказку (W12).
	if strings.Contains(openHTML, "Метрика 0") || strings.Contains(openHTML, "SLO 0") {
		t.Errorf("composition hint must omit zero-count sources: %s", openHTML)
	}
	if !strings.Contains(openHTML, "gw-1") {
		t.Errorf("open group card missing root name: %s", openHTML)
	}

	resolvedAt := time.Now()
	closed := open
	closed.Group.ResolvedAt = &resolvedAt
	closedHTML := renderTo(t, groupCard(1, closed, true))
	if !strings.Contains(closedHTML, "решена") {
		t.Error("resolved group card must show the resolved badge")
	}
}

// TestGroupCardEmptyMembers — группа без состава (гипотетически, для
// устойчивости шаблона к пустому срезу): рендерится без паники, таблица без
// строк.
func TestGroupCardEmptyMembers(t *testing.T) {
	c := NewGroupCard(incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "uptime", StartedAt: time.Now()}}, nil)
	html := renderTo(t, groupCard(1, c, true))
	if !strings.Contains(html, "всего 0 (—)") {
		t.Errorf("empty group card composition hint wrong: %s", html)
	}
	if strings.Contains(html, "<tbody><tr>") {
		t.Errorf("empty group card must not render any member row: %s", html)
	}
}

// TestFeedItemRowBadgesAndSubKind — feedItemRow: SubKind-подсказка,
// suppressed-by-dep и acked-бейджи рисуются независимо друг от друга и
// только когда соответствующее поле выставлено; severity-бейдж не рисуется
// при пустой Severity.
func TestFeedItemRowBadgesAndSubKind(t *testing.T) {
	base := incidentgroup.FeedItem{Source: "host", Title: "web-1", StartedAt: time.Now()}

	plain := renderTo(t, feedItemRow(1, base, nil, true))
	if strings.Contains(plain, "·") {
		t.Errorf("row without SubKind must not render the hint separator: %s", plain)
	}
	if strings.Contains(plain, "подавлен: родитель недоступен") || strings.Contains(plain, "Подтверждён") {
		t.Errorf("plain row must not show suppression/ack badges: %s", plain)
	}

	withSubKind := base
	withSubKind.SubKind = "disk"
	subKindHTML := renderTo(t, feedItemRow(1, withSubKind, nil, true))
	// W13: SubKind рисуется переведённым (hosts.kind.disk), не сырым
	// значением из БД.
	if !strings.Contains(subKindHTML, "· Диск") {
		t.Errorf("row with SubKind must render the translated hint: %s", subKindHTML)
	}
	if strings.Contains(subKindHTML, "· disk") {
		t.Errorf("row with SubKind must NOT render the raw untranslated value: %s", subKindHTML)
	}

	suppressed := base
	suppressed.SuppressedByDep = true
	suppressedHTML := renderTo(t, feedItemRow(1, suppressed, nil, true))
	if !strings.Contains(suppressedHTML, "подавлен: родитель недоступен") {
		t.Errorf("suppressed row must show the badge: %s", suppressedHTML)
	}
	if strings.Contains(suppressedHTML, "Подтверждён") {
		t.Errorf("suppressed-only row must not show the ack badge: %s", suppressedHTML)
	}

	acked := base
	acked.Acknowledged = true
	ackedHTML := renderTo(t, feedItemRow(1, acked, nil, true))
	if !strings.Contains(ackedHTML, "Подтверждён") {
		t.Errorf("acked row must show the ack badge: %s", ackedHTML)
	}
	if strings.Contains(ackedHTML, "подавлен: родитель недоступен") {
		t.Errorf("acked-only row must not show the suppression badge: %s", ackedHTML)
	}

	both := base
	both.SuppressedByDep = true
	both.Acknowledged = true
	bothHTML := renderTo(t, feedItemRow(1, both, nil, true))
	if !strings.Contains(bothHTML, "подавлен: родитель недоступен") || !strings.Contains(bothHTML, "Подтверждён") {
		t.Errorf("row with both flags must show both badges: %s", bothHTML)
	}
}

// TestFeedItemRowSeverityBadge — Severity="" — бейдж severity не рисуется
// вовсе; непустая Severity — рисуется (класс danger для critical).
func TestFeedItemRowSeverityBadge(t *testing.T) {
	noSeverity := renderTo(t, feedItemRow(1, incidentgroup.FeedItem{Source: "host", StartedAt: time.Now()}, nil, true))
	if strings.Contains(noSeverity, "badge-danger") || strings.Contains(noSeverity, "badge-warn") {
		t.Errorf("row without severity must not render a severity badge: %s", noSeverity)
	}

	critical := renderTo(t, feedItemRow(1, incidentgroup.FeedItem{Source: "host", Severity: "critical", StartedAt: time.Now()}, nil, true))
	if !strings.Contains(critical, "badge-danger") {
		t.Errorf("critical severity must render the danger badge: %s", critical)
	}
}

// TestIncidentFeedOpenGroupsSection — секция «Открытые группы» непуста:
// карточка рендерится, заглушка «Открытых групп нет» отсутствует.
func TestIncidentFeedOpenGroupsSection(t *testing.T) {
	group := NewGroupCard(
		incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: time.Now()}},
		[]incidentgroup.FeedItem{{Source: "host"}},
	)
	html := renderTo(t, Overview(1, "24h", []GroupCard{group}, nil, nil, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	if strings.Contains(html, "Открытых групп нет") {
		t.Errorf("non-empty openGroups must not render the empty-state message: %s", html)
	}
	if !strings.Contains(html, "Хост недоступен") {
		t.Errorf("open group card must render its root label: %s", html)
	}
	// Остальные две секции по-прежнему пусты.
	if !strings.Contains(html, "Открытых инцидентов вне групп нет") {
		t.Errorf("out-of-group section must still show its empty state: %s", html)
	}
	if !strings.Contains(html, "Недавно ничего не решалось") {
		t.Errorf("closed section must still show its empty state: %s", html)
	}
}

// TestIncidentFeedOutOfGroupAllSixSources — все 6 источников во «вне групп»
// рендерятся строками таблицы с локализованной подписью источника.
func TestIncidentFeedOutOfGroupAllSixSources(t *testing.T) {
	sources := []string{"host", "uptime", "metric", "slo", "trace", "profile"}
	labels := map[string]string{
		"host": "Хост", "uptime": "Аптайм", "metric": "Метрика",
		"slo": "SLO", "trace": "Транзакции", "profile": "Профилирование",
	}
	items := make([]incidentgroup.FeedItem, 0, len(sources))
	for i, src := range sources {
		items = append(items, incidentgroup.FeedItem{
			Source: src, IncidentID: int64(i + 1), Title: "item-" + src, StartedAt: time.Now(),
		})
	}
	html := renderTo(t, Overview(1, "24h", nil, items, nil, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	if strings.Contains(html, "Открытых инцидентов вне групп нет") {
		t.Errorf("non-empty outOfGroup must not render the empty-state message: %s", html)
	}
	for _, src := range sources {
		if !strings.Contains(html, labels[src]) {
			t.Errorf("out-of-group table missing localized label for source %q (%s): %s", src, labels[src], html)
		}
		if !strings.Contains(html, "item-"+src) {
			t.Errorf("out-of-group table missing title for source %q: %s", src, html)
		}
	}
}

// TestIncidentFeedClosedGroupsWithoutOutOfGroupItems — closedGroups непуст,
// но closed (внегрупповые закрытые) пуст: карточки закрытых групп рисуются,
// но НИ заглушка «ничего не решалось», НИ отдельная таблица внегрупповых
// закрытых не рисуются (ветка else-if len(closed)>0 не срабатывает).
func TestIncidentFeedClosedGroupsWithoutOutOfGroupItems(t *testing.T) {
	resolvedAt := time.Now()
	closedGroup := NewGroupCard(
		incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "uptime", StartedAt: time.Now(), ResolvedAt: &resolvedAt}},
		[]incidentgroup.FeedItem{{Source: "metric"}},
	)
	html := renderTo(t, Overview(1, "24h", nil, nil, []GroupCard{closedGroup}, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	if strings.Contains(html, "Недавно ничего не решалось") {
		t.Errorf("closedGroups>0 must suppress the closed-empty message even though out-of-group closed items are empty: %s", html)
	}
	if !strings.Contains(html, "Монитор недоступен") {
		t.Errorf("closed group card must render: %s", html)
	}
	if !strings.Contains(html, "решена") {
		t.Errorf("closed group card must show the resolved badge: %s", html)
	}
}

// TestIncidentFeedClosedOutOfGroupWithoutGroups — обратная комбинация:
// closedGroups пуст, closed (внегрупповые) непуст — таблица внегрупповых
// закрытых рисуется, заглушка отсутствует, карточек групп нет.
func TestIncidentFeedClosedOutOfGroupWithoutGroups(t *testing.T) {
	closed := []incidentgroup.FeedItem{{Source: "host", Title: "closed-lone", StartedAt: time.Now()}}
	html := renderTo(t, Overview(1, "24h", nil, nil, nil, closed, FeedCaps{}, true, StatusLine{}, nil, ""))
	if strings.Contains(html, "Недавно ничего не решалось") {
		t.Errorf("non-empty closed out-of-group items must suppress the empty message: %s", html)
	}
	if !strings.Contains(html, "closed-lone") {
		t.Errorf("closed out-of-group table must render the item: %s", html)
	}
}

// TestIncidentFeedProjectIDInLinks — projectID прокидывается в ссылки строк
// (feedItemHref), а не теряется по пути в IncidentFeed -> feedItemRow.
func TestIncidentFeedProjectIDInLinks(t *testing.T) {
	const projectID = int64(777)
	items := []incidentgroup.FeedItem{{Source: "metric", Title: "m1", StartedAt: time.Now()}}
	html := renderTo(t, Overview(projectID, "24h", nil, items, nil, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	want := metricAlertsBasePath(projectID)
	if !strings.Contains(html, want) {
		t.Errorf("row link must use the page's projectID (%d): missing %q in %s", projectID, want, html)
	}
	if strings.Contains(html, metricAlertsBasePath(projectID+1)) {
		t.Errorf("row link must not use an unrelated projectID: %s", html)
	}
}

// TestFeedItemHrefHostReusesHostLink — W23: ссылка на хост-источник обязана
// приходить из hostLink (hosts.templ), а не из собственного дубля пути —
// иначе побитовое совпадение с TestFeedItemHrefKnownSources было бы просто
// совпадением значений, а не доказательством переиспользования.
func TestFeedItemHrefHostReusesHostLink(t *testing.T) {
	const projectID = int64(9)
	item := incidentgroup.FeedItem{Source: "host", RefName: "db/replica 1"}
	got := feedItemHref(projectID, item)
	want := hostLink(projectID, item.RefName)
	if got != want {
		t.Errorf("feedItemHref(host) = %q, want hostLink() output %q", got, want)
	}
}

// TestOverviewHelpPanelAndBackLink — W16/W28: страница объясняет себя
// (helpPanel со ссылкой на /docs/incident-groups) и ведёт обратно на
// страницу аптайм-инцидентов (/projects/{id}/incidents), а не только
// наоборот. Подпись ссылки — "Сбои доступности" (nav.incidents), не
// "Инциденты": прежняя строка ассерта уже разъехалась с каталогом
// (nav.incidents переименован в более раннем разделе фичи) и была
// самотавтологичной находкой — здесь она литералом, попутно с переносом
// теста на Overview.
func TestOverviewHelpPanelAndBackLink(t *testing.T) {
	const projectID = int64(5)
	html := renderTo(t, Overview(projectID, "24h", nil, nil, nil, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	if !strings.Contains(html, `class="help-panel"`) {
		t.Errorf("page must render the help panel: %s", html)
	}
	if !strings.Contains(html, `href="/docs/incident-groups"`) {
		t.Errorf("help panel must link to the incident-groups guide: %s", html)
	}
	wantBackHref := `href="` + incidentsPath(projectID) + `"`
	if !strings.Contains(html, wantBackHref) {
		t.Errorf("feed must link back to %s: missing %q in %s", incidentsPath(projectID), wantBackHref, html)
	}
	if !strings.Contains(html, "Сбои доступности</a>") {
		t.Errorf("back link text must be the localized nav.incidents label: %s", html)
	}
}

// TestIncidentFeedTableHeadColumns — W19: таблицы состава группы, вне групп
// и закрытых внегрупповых обязаны нести подписи колонок (thead), а не
// голые 4 колонки без заголовка.
func TestIncidentFeedTableHeadColumns(t *testing.T) {
	group := NewGroupCard(
		incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: time.Now()}},
		[]incidentgroup.FeedItem{{Source: "host"}},
	)
	outOfGroup := []incidentgroup.FeedItem{{Source: "uptime", Title: "m1", StartedAt: time.Now()}}
	closed := []incidentgroup.FeedItem{{Source: "metric", Title: "m2", StartedAt: time.Now()}}
	html := renderTo(t, Overview(1, "24h", []GroupCard{group}, outOfGroup, nil, closed, FeedCaps{}, true, StatusLine{}, nil, ""))

	wantHeaders := []string{"Источник", "Название", "Статус", "Начало", "Решено"}
	// 3 непустых таблицы (состав группы, вне групп, закрытые) — по одной
	// шапке на каждую: считаем вхождения ровно 3 на колонку, не "хотя бы
	// одна", иначе мутация, потерявшая @feedTableHead в одном из трёх мест,
	// прошла бы незамеченной.
	for _, h := range wantHeaders {
		want := `<th scope="col">` + h + `</th>`
		if got := strings.Count(html, want); got != 3 {
			t.Errorf("column header %q: found %d times, want 3 (group/out-of-group/closed): %s", h, got, html)
		}
	}
	// Мутация, снимающая контейнер <thead>/</thead> и оставляющая голые
	// <th>, не должна проходить незамеченной: считаем сам контейнер отдельно
	// от подписей колонок, ровно 3 раза (по таблице на секцию).
	for _, tag := range []string{"<thead>", "</thead>"} {
		if got := strings.Count(html, tag); got != 3 {
			t.Errorf("%s: found %d times, want 3 (group/out-of-group/closed): %s", tag, got, html)
		}
	}
}

// TestIncidentFeedDistinctAriaLabels — W20: скролл-регионы состава группы,
// внегрупповых и закрытых обязаны иметь РАЗНЫЕ aria-label, иначе диктор не
// отличает их друг от друга (раньше все три несли "Лента инцидентов").
func TestIncidentFeedDistinctAriaLabels(t *testing.T) {
	group := NewGroupCard(
		incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: time.Now()}},
		[]incidentgroup.FeedItem{{Source: "host"}},
	)
	outOfGroup := []incidentgroup.FeedItem{{Source: "uptime", Title: "m1", StartedAt: time.Now()}}
	closed := []incidentgroup.FeedItem{{Source: "metric", Title: "m2", StartedAt: time.Now()}}
	html := renderTo(t, Overview(1, "24h", []GroupCard{group}, outOfGroup, nil, closed, FeedCaps{}, true, StatusLine{}, nil, ""))

	groupLabel := `aria-label="Состав группы"`
	outLabel := `aria-label="Вне групп"`
	closedLabelWanted := `aria-label="Недавно решённые"`
	for _, want := range []string{groupLabel, outLabel, closedLabelWanted} {
		if !strings.Contains(html, want) {
			t.Errorf("missing scroll region %s: %s", want, html)
		}
	}
	if strings.Contains(html, `aria-label="Лента инцидентов"`) {
		t.Errorf("no scroll region should still carry the old shared page-title label: %s", html)
	}
}

// TestIncidentFeedEmptyStatesUseEmptyStateComponent — W18: пустые секции
// рендерятся через @emptyState (иконка + заголовок + текст), а не голым
// <p class="hint">.
func TestOverviewPartialEmptySectionsUseEmptyStateComponent(t *testing.T) {
	openGroups := []GroupCard{NewGroupCard(
		incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: time.Now()}},
		[]incidentgroup.FeedItem{{Source: "host"}},
	)}
	html := renderTo(t, Overview(1, "24h", openGroups, nil, nil, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	if got := strings.Count(html, `class="empty-state"`); got != 2 {
		t.Errorf("the 2 empty sections (out-of-group, closed) must render @emptyState: got %d, want 2: %s", got, html)
	}
	if got := strings.Count(html, `href="#i-activity"`); got != 2 {
		t.Errorf("empty sections must use the activity icon: got %d uses, want 2: %s", got, html)
	}
	// Единственный оставшийся <p class="hint"> — ссылка назад на /incidents
	// под <h1> (не относится к пустым состояниям); если бы эмпти-стейты
	// откатились на старый голый абзац, счёт вырос бы до 3.
	if got := strings.Count(html, `<p class="hint">`); got != 1 {
		t.Errorf("only the back-link hint paragraph should remain, empty sections must use @emptyState: got %d <p class=\"hint\"> blocks, want 1: %s", got, html)
	}
}

// TestOverviewEmptyProjectShowsGettingStartedInvite — задача 6 nav-ia:
// проект без единого инцидента ни в одной из четырёх выборок получает не три
// «нет данных» подряд (выглядело бы как поломка на первом же экране
// проекта), а одно приглашение подключить SDK со ссылкой на «Первые шаги».
func TestOverviewEmptyProjectShowsGettingStartedInvite(t *testing.T) {
	const projectID = int64(11)
	html := renderTo(t, Overview(projectID, "24h", nil, nil, nil, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	if got := strings.Count(html, `class="empty-state"`); got != 1 {
		t.Errorf("a totally empty project must render exactly ONE page-level empty state, not per-section: got %d: %s", got, html)
	}
	if !strings.Contains(html, `href="#i-home"`) {
		t.Errorf("the page-level empty state must use the home icon (same as the rail area): %s", html)
	}
	if strings.Contains(html, `href="#i-activity"`) {
		t.Errorf("no per-section empty state should render alongside the page-level one: %s", html)
	}
	wantCTA := `href="` + projectSetupPath(projectID) + `"`
	if !strings.Contains(html, wantCTA) {
		t.Errorf("empty state must link to the project's getting-started page: missing %q in %s", wantCTA, html)
	}
}

// TestFeedItemSubKindTranslatesTraceMetric — W13: trace-подвид (perf_regressions.metric)
// рисуется через regressionMetricLabel (тот же помощник, что и regressions.templ),
// не сырым значением metric.
func TestFeedItemSubKindTranslatesTraceMetric(t *testing.T) {
	item := incidentgroup.FeedItem{Source: "trace", Title: "t1", SubKind: "duration", StartedAt: time.Now()}
	html := renderTo(t, feedItemRow(1, item, nil, true))
	if !strings.Contains(html, "· p95 длительности") {
		t.Errorf("trace SubKind must render via regressionMetricLabel: %s", html)
	}
	if strings.Contains(html, "· duration") {
		t.Errorf("trace row must not render the raw untranslated metric key: %s", html)
	}
}

// TestFeedItemSubKindProfilePassesThroughRaw — W13: profile_type нигде в
// продукте не переводится (profileregressions.templ печатает его как есть)
// — лента обязана быть с этим согласована, а не внезапно завести перевод,
// которого нет на родной странице источника.
func TestFeedItemSubKindProfilePassesThroughRaw(t *testing.T) {
	item := incidentgroup.FeedItem{Source: "profile", Title: "svc", SubKind: "cpu", StartedAt: time.Now()}
	html := renderTo(t, feedItemRow(1, item, nil, true))
	if !strings.Contains(html, "· cpu") {
		t.Errorf("profile SubKind must render as-is: %s", html)
	}
}

// TestFeedItemRowHeldByGroupBadge — W15: бейдж «молчит — уведомляет корень»
// рисуется только при HeldByGroup=true, независимо от SuppressedByDep (уже
// покрыт TestFeedItemRowBadgesAndSubKind) — оба могут стоять одновременно,
// ни один не подменяет другой.
func TestFeedItemRowHeldByGroupBadge(t *testing.T) {
	base := incidentgroup.FeedItem{Source: "metric", Title: "m1", StartedAt: time.Now()}
	plainHTML := renderTo(t, feedItemRow(1, base, nil, true))
	if strings.Contains(plainHTML, "молчит — уведомляет корень") {
		t.Errorf("row without HeldByGroup must not render the badge: %s", plainHTML)
	}

	held := base
	held.HeldByGroup = true
	heldHTML := renderTo(t, feedItemRow(1, held, nil, true))
	if !strings.Contains(heldHTML, "молчит — уведомляет корень") {
		t.Errorf("HeldByGroup row must render the badge: %s", heldHTML)
	}

	both := held
	both.SuppressedByDep = true
	bothHTML := renderTo(t, feedItemRow(1, both, nil, true))
	if !strings.Contains(bothHTML, "молчит — уведомляет корень") || !strings.Contains(bothHTML, "подавлен: родитель недоступен") {
		t.Errorf("row with both HeldByGroup and SuppressedByDep must show both distinct badges: %s", bothHTML)
	}
}

// TestFeedItemRowResolvedBadgeAndTime — W14: закрывшийся член ОТКРЫТОЙ
// группы обязан нести и бейдж «Решён», и время закрытия — не только время
// начала. Проверяем через datetime-атрибут relativeTime (детерминированный,
// в отличие от "N часов назад").
func TestFeedItemRowResolvedBadgeAndTime(t *testing.T) {
	started := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	resolved := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedDT := `datetime="2026-01-01T10:00:00Z"`
	resolvedDT := `datetime="2026-01-01T12:00:00Z"`

	open := incidentgroup.FeedItem{Source: "host", StartedAt: started}
	openHTML := renderTo(t, feedItemRow(1, open, nil, true))
	if !strings.Contains(openHTML, startedDT) {
		t.Errorf("open member row must show its started time: %s", openHTML)
	}
	if strings.Contains(openHTML, "Решён") {
		t.Errorf("open member row must not show the resolved badge: %s", openHTML)
	}

	resolvedItem := open
	resolvedItem.ResolvedAt = &resolved
	resolvedHTML := renderTo(t, feedItemRow(1, resolvedItem, nil, true))
	if !strings.Contains(resolvedHTML, "Решён") {
		t.Errorf("resolved member row must show the resolved badge: %s", resolvedHTML)
	}
	if !strings.Contains(resolvedHTML, startedDT) {
		t.Errorf("resolved member row must still show its started time: %s", resolvedHTML)
	}
	if !strings.Contains(resolvedHTML, resolvedDT) {
		t.Errorf("resolved member row must show its resolved time too, not just started: %s", resolvedHTML)
	}
	// K9-14: начало и закрытие — в РАЗНЫХ ячейках под своими заголовками, а
	// не два относительных времени подряд в одной («3 дня назадтолько что»).
	// Между двумя datetime обязан стоять закрывающий </td>; у открытой
	// строки та же пятая ячейка есть, но пустая — ширина таблицы не пляшет.
	for name, html := range map[string]string{"open": openHTML, "resolved": resolvedHTML} {
		if got := strings.Count(html, "<td"); got != 5 {
			t.Errorf("%s row: %d cells, want 5 (source/title/status/started/resolved): %s", name, got, html)
		}
	}
	between := resolvedHTML[strings.Index(resolvedHTML, startedDT):strings.Index(resolvedHTML, resolvedDT)]
	if !strings.Contains(between, "</td>") {
		t.Errorf("started and resolved times must sit in separate cells, got them glued in one: %s", resolvedHTML)
	}
}

// TestFeedItemRowWasGroupedBadge — бейдж «была в группе» рисуется только
// при FormerGroupID != 0; ссылка на якорь карточки — только когда её ID
// входит в closedGroupIDs (карточка реально отрендерена на странице), иначе
// голый текст без ссылки; пустое FormerGroupRootName (группа удалена
// janitor'ом целиком) переиспользует тот же фолбэк, что и W22.
func TestFeedItemRowWasGroupedBadge(t *testing.T) {
	plain := incidentgroup.FeedItem{Source: "host", Title: "m1", StartedAt: time.Now()}
	plainHTML := renderTo(t, feedItemRow(1, plain, nil, true))
	if strings.Contains(plainHTML, "ранее в группе") {
		t.Errorf("row without FormerGroupID must not render the was_grouped badge: %s", plainHTML)
	}

	former := plain
	former.FormerGroupID = 7
	former.FormerGroupRootName = "root-1"
	htmlNoLink := renderTo(t, feedItemRow(1, former, nil, true))
	if !strings.Contains(htmlNoLink, "ранее в группе — root-1") {
		t.Errorf("row with FormerGroupID must render the was_grouped badge: %s", htmlNoLink)
	}
	if strings.Contains(htmlNoLink, `href="#group-7"`) {
		t.Errorf("badge must not link when the group card is not rendered on this page: %s", htmlNoLink)
	}

	htmlWithLink := renderTo(t, feedItemRow(1, former, map[int64]bool{7: true}, true))
	if !strings.Contains(htmlWithLink, `href="#group-7"`) {
		t.Errorf("badge must link to the rendered group card by anchor: %s", htmlWithLink)
	}

	purged := plain
	purged.FormerGroupID = 9
	purged.FormerGroupRootName = ""
	purgedHTML := renderTo(t, feedItemRow(1, purged, nil, true))
	if !strings.Contains(purgedHTML, "ранее в группе — узел удалён") {
		t.Errorf("a purged former group must fall back to the deleted-node label: %s", purgedHTML)
	}
}

// TestGroupCardRootSeverityBadge — W24: severity-бейдж корня в шапке
// карточки; пустая RootSeverity (uptime-корень, у incidents нет колонки
// severity) бейдж не рисует вовсе.
func TestGroupCardRootSeverityBadge(t *testing.T) {
	critical := NewGroupCard(incidentgroup.GroupRow{
		Group:        incidentgroup.Group{RootSource: "host", StartedAt: time.Now()},
		RootSeverity: "critical",
	}, nil)
	html := renderTo(t, groupCard(1, critical, true))
	if !strings.Contains(html, "badge-danger") {
		t.Errorf("group card must render the root severity badge: %s", html)
	}

	noSeverity := NewGroupCard(incidentgroup.GroupRow{
		Group: incidentgroup.Group{RootSource: "uptime", StartedAt: time.Now()},
	}, nil)
	htmlNoSeverity := renderTo(t, groupCard(1, noSeverity, true))
	if strings.Contains(htmlNoSeverity, "badge-danger") || strings.Contains(htmlNoSeverity, "badge-warn") {
		t.Errorf("group card without root severity must not render a severity badge: %s", htmlNoSeverity)
	}
}

// TestGroupCardShowsResolvedTimeNotStartedAt — W21: закрытая группа
// показывает в шапке время ЗАКРЫТИЯ (тот же ключ, по которому отсортирована
// секция «закрытые» — ClosedGroupsSince ORDER BY resolved_at DESC), а не
// время начала — иначе порядок карточек на странице выглядит случайным.
func TestGroupCardShowsResolvedTimeNotStartedAt(t *testing.T) {
	started := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	resolved := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startedDT := `datetime="2026-01-01T10:00:00Z"`
	resolvedDT := `datetime="2026-01-01T12:00:00Z"`

	open := NewGroupCard(incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: started}}, nil)
	openHTML := renderTo(t, groupCard(1, open, true))
	if !strings.Contains(openHTML, startedDT) {
		t.Errorf("open group card header must show the started time: %s", openHTML)
	}

	closed := NewGroupCard(incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "host", StartedAt: started, ResolvedAt: &resolved}}, nil)
	closedHTML := renderTo(t, groupCard(1, closed, true))
	if !strings.Contains(closedHTML, resolvedDT) {
		t.Errorf("closed group card header must show the resolved time: %s", closedHTML)
	}
	if strings.Contains(closedHTML, startedDT) {
		t.Errorf("closed group card header must NOT show the started time (sort key mismatch, W21): %s", closedHTML)
	}
}

// TestGroupCardDeletedRootFallback — W22: корень с пустым RootName (узел
// удалён) обязан показать текстовый фолбэк, а не пустую строку, и не
// рисовать вырожденную ссылку (host — на /projects/{id}/hosts/, monitor —
// на несуществующую карточку).
func TestGroupCardDeletedRootFallback(t *testing.T) {
	c := NewGroupCard(incidentgroup.GroupRow{
		Group:    incidentgroup.Group{RootSource: "host", RootNodeKind: "host", StartedAt: time.Now()},
		RootName: "",
	}, nil)
	html := renderTo(t, groupCard(1, c, true))
	if !strings.Contains(html, "узел удалён") {
		t.Errorf("deleted root must show the fallback label: %s", html)
	}
	if strings.Contains(html, `href="`+hostsBasePath(1)+`/"`) {
		t.Errorf("deleted root must NOT render a degenerate empty-name link: %s", html)
	}
}

// TestFeedItemRowDeletedMemberFallback — W22 (симметрия для члена группы,
// а не только корня): пустой it.Title обязан показать текстовый фолбэк
// (тот же feed.group.root_deleted, что и у корня) и не рисовать вырожденную
// ссылку с пустым текстом (<a href="...">< /a>). На проде недостижимо (FK
// ON DELETE CASCADE у всех 4 источников-членов, см. докблок feedMemberSelect
// в group.go), но рендер обязан оставаться защитным.
func TestFeedItemRowDeletedMemberFallback(t *testing.T) {
	it := incidentgroup.FeedItem{Source: "host", Title: "", RefName: "", StartedAt: time.Now()}
	html := renderTo(t, feedItemRow(1, it, nil, true))
	if !strings.Contains(html, "узел удалён") {
		t.Errorf("deleted member must show the fallback label: %s", html)
	}
	if strings.Contains(html, `href="`+hostsBasePath(1)+`/"><`) {
		t.Errorf("deleted member must NOT render a degenerate empty-name link: %s", html)
	}
	if strings.Contains(html, "<a ") {
		t.Errorf("deleted member must not render a link at all: %s", html)
	}
}

// TestGroupCardRootNameLinksToHostDetail — W22 (positive case): непустое
// имя корня — ссылка на родную страницу узла (hostLink, тот же билдер, что
// и feedItemHref/hosts.templ), не голый текст.
func TestGroupCardRootNameLinksToHostDetail(t *testing.T) {
	c := NewGroupCard(incidentgroup.GroupRow{
		Group:    incidentgroup.Group{RootSource: "host", RootNodeKind: "host", StartedAt: time.Now()},
		RootName: "gw-1",
	}, nil)
	html := renderTo(t, groupCard(1, c, true))
	want := `href="` + hostLink(1, "gw-1") + `"`
	if !strings.Contains(html, want) {
		t.Errorf("group card root name must link via hostLink: missing %q in %s", want, html)
	}
}

// TestIncidentFeedWasGroupedBadgeLinksToRenderedClosedGroup — интеграционно
// (уровень IncidentFeed, не голого feedItemRow): closedGroupIDSet реально
// прокидывается из closedGroups в строки «вне групп», а не теряется по
// дороге — иначе TestFeedItemRowWasGroupedBadge проверял бы helper в
// вакууме, а не то, что видит пользователь на странице.
func TestIncidentFeedWasGroupedBadgeLinksToRenderedClosedGroup(t *testing.T) {
	closedGroup := NewGroupCard(
		incidentgroup.GroupRow{Group: incidentgroup.Group{ID: 42, RootSource: "host", StartedAt: time.Now()}, RootName: "root-1"},
		nil,
	)
	outOfGroup := []incidentgroup.FeedItem{{
		Source: "host", Title: "m1", StartedAt: time.Now(),
		FormerGroupID: 42, FormerGroupRootName: "root-1",
	}}
	html := renderTo(t, Overview(1, "24h", nil, outOfGroup, []GroupCard{closedGroup}, nil, FeedCaps{}, true, StatusLine{}, nil, ""))
	if !strings.Contains(html, `id="group-42"`) {
		t.Errorf("closed group card must carry the anchor id: %s", html)
	}
	if !strings.Contains(html, `href="#group-42"`) {
		t.Errorf("was_grouped badge must link to the rendered closed group card: %s", html)
	}
}

// TestFeedItemLinkable — W9: только metric/slo зависят от canOperate. Оба
// источника ведут на lvlOperator-страницы (/projects/{id}/metrics/alerts,
// /projects/{id}/slos — authz_map_test.go), лента же отдаётся любому
// участнику проекта с доступом (lvlAccess) — без гейта рядовой участник
// видел бы ссылку, закрывающуюся 404. Остальные 4 источника ведут на
// lvlAccess-страницы и не зависят от canOperate вовсе.
func TestFeedItemLinkable(t *testing.T) {
	cases := []struct {
		source              string
		linkableOperator    bool
		linkableNonOperator bool
	}{
		{"host", true, true},
		{"uptime", true, true},
		{"metric", true, false},
		{"slo", true, false},
		{"trace", true, true},
		{"profile", true, true},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			it := incidentgroup.FeedItem{Source: c.source}
			if got := feedItemLinkable(it, true); got != c.linkableOperator {
				t.Errorf("feedItemLinkable(%s, canOperate=true) = %v, want %v", c.source, got, c.linkableOperator)
			}
			if got := feedItemLinkable(it, false); got != c.linkableNonOperator {
				t.Errorf("feedItemLinkable(%s, canOperate=false) = %v, want %v", c.source, got, c.linkableNonOperator)
			}
		})
	}
}

// TestFeedItemRowHidesOperatorOnlyLinkForNonOperator — рендер-уровень W9:
// non-operator видит имя metric/slo-инцидента как голый текст, БЕЗ <a href>
// на operator-страницу; host виден со ссылкой независимо от canOperate
// (его родная страница — lvlAccess, canOperate её не касается).
func TestFeedItemRowHidesOperatorOnlyLinkForNonOperator(t *testing.T) {
	const projectID = int64(7)
	metricItem := incidentgroup.FeedItem{Source: "metric", Title: "cpu.load rule", StartedAt: time.Now()}

	nonOperatorHTML := renderTo(t, feedItemRow(projectID, metricItem, nil, false))
	if strings.Contains(nonOperatorHTML, "<a href") {
		t.Errorf("non-operator row for a metric incident must not render any link: %s", nonOperatorHTML)
	}
	if !strings.Contains(nonOperatorHTML, "cpu.load rule") {
		t.Errorf("non-operator row must still show the incident title as plain text: %s", nonOperatorHTML)
	}

	operatorHTML := renderTo(t, feedItemRow(projectID, metricItem, nil, true))
	if !strings.Contains(operatorHTML, `<a href="`+metricAlertsBasePath(projectID)+`"`) {
		t.Errorf("operator row for a metric incident must link to the alerts page: %s", operatorHTML)
	}

	hostItem := incidentgroup.FeedItem{Source: "host", Title: "web-1", RefName: "web-1", StartedAt: time.Now()}
	hostNonOperatorHTML := renderTo(t, feedItemRow(projectID, hostItem, nil, false))
	if !strings.Contains(hostNonOperatorHTML, "<a href") {
		t.Errorf("host row must keep its link regardless of canOperate (its own page is lvlAccess): %s", hostNonOperatorHTML)
	}
}
