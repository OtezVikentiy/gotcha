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
		{"slo", incidentgroup.FeedItem{Source: "slo"}, SLOsPath(projectID)},
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

// TestFeedItemHrefUnknownSourceFallsBackToFeedPath — источник, не входящий
// ни в одну из 6 известных веток, обязан упасть в default: ссылка на саму
// ленту (incidentFeedPath), а не паника/пустая строка.
func TestFeedItemHrefUnknownSourceFallsBackToFeedPath(t *testing.T) {
	const projectID = int64(42)
	got := feedItemHref(projectID, incidentgroup.FeedItem{Source: "bogus"})
	want := incidentFeedPath(projectID)
	if got != want {
		t.Errorf("feedItemHref(unknown source) = %q, want %q", got, want)
	}
	if want != "/projects/42/incident-feed" {
		t.Errorf("incidentFeedPath(42) = %q, want /projects/42/incident-feed", want)
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
	openHTML := renderTo(t, groupCard(1, open))
	if strings.Contains(openHTML, "решена") {
		t.Error("open group card must not show the resolved badge")
	}
	if !strings.Contains(openHTML, "host 1 · uptime 1 · metric 0 · slo 0") {
		t.Errorf("open group card missing composition hint: %s", openHTML)
	}
	if !strings.Contains(openHTML, "gw-1") {
		t.Errorf("open group card missing root name: %s", openHTML)
	}

	resolvedAt := time.Now()
	closed := open
	closed.Group.ResolvedAt = &resolvedAt
	closedHTML := renderTo(t, groupCard(1, closed))
	if !strings.Contains(closedHTML, "решена") {
		t.Error("resolved group card must show the resolved badge")
	}
}

// TestGroupCardEmptyMembers — группа без состава (гипотетически, для
// устойчивости шаблона к пустому срезу): рендерится без паники, таблица без
// строк.
func TestGroupCardEmptyMembers(t *testing.T) {
	c := NewGroupCard(incidentgroup.GroupRow{Group: incidentgroup.Group{RootSource: "uptime", StartedAt: time.Now()}}, nil)
	html := renderTo(t, groupCard(1, c))
	if !strings.Contains(html, "host 0 · uptime 0 · metric 0 · slo 0") {
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

	plain := renderTo(t, feedItemRow(1, base))
	if strings.Contains(plain, "·") {
		t.Errorf("row without SubKind must not render the hint separator: %s", plain)
	}
	if strings.Contains(plain, "подавлен: родитель недоступен") || strings.Contains(plain, "Подтверждён") {
		t.Errorf("plain row must not show suppression/ack badges: %s", plain)
	}

	withSubKind := base
	withSubKind.SubKind = "disk"
	subKindHTML := renderTo(t, feedItemRow(1, withSubKind))
	if !strings.Contains(subKindHTML, "· disk") {
		t.Errorf("row with SubKind must render the hint: %s", subKindHTML)
	}

	suppressed := base
	suppressed.SuppressedByDep = true
	suppressedHTML := renderTo(t, feedItemRow(1, suppressed))
	if !strings.Contains(suppressedHTML, "подавлен: родитель недоступен") {
		t.Errorf("suppressed row must show the badge: %s", suppressedHTML)
	}
	if strings.Contains(suppressedHTML, "Подтверждён") {
		t.Errorf("suppressed-only row must not show the ack badge: %s", suppressedHTML)
	}

	acked := base
	acked.Acknowledged = true
	ackedHTML := renderTo(t, feedItemRow(1, acked))
	if !strings.Contains(ackedHTML, "Подтверждён") {
		t.Errorf("acked row must show the ack badge: %s", ackedHTML)
	}
	if strings.Contains(ackedHTML, "подавлен: родитель недоступен") {
		t.Errorf("acked-only row must not show the suppression badge: %s", ackedHTML)
	}

	both := base
	both.SuppressedByDep = true
	both.Acknowledged = true
	bothHTML := renderTo(t, feedItemRow(1, both))
	if !strings.Contains(bothHTML, "подавлен: родитель недоступен") || !strings.Contains(bothHTML, "Подтверждён") {
		t.Errorf("row with both flags must show both badges: %s", bothHTML)
	}
}

// TestFeedItemRowSeverityBadge — Severity="" — бейдж severity не рисуется
// вовсе; непустая Severity — рисуется (класс danger для critical).
func TestFeedItemRowSeverityBadge(t *testing.T) {
	noSeverity := renderTo(t, feedItemRow(1, incidentgroup.FeedItem{Source: "host", StartedAt: time.Now()}))
	if strings.Contains(noSeverity, "badge-danger") || strings.Contains(noSeverity, "badge-warn") {
		t.Errorf("row without severity must not render a severity badge: %s", noSeverity)
	}

	critical := renderTo(t, feedItemRow(1, incidentgroup.FeedItem{Source: "host", Severity: "critical", StartedAt: time.Now()}))
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
	html := renderTo(t, IncidentFeed(1, []GroupCard{group}, nil, nil, nil, ""))
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
	html := renderTo(t, IncidentFeed(1, nil, items, nil, nil, ""))
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
	html := renderTo(t, IncidentFeed(1, nil, nil, []GroupCard{closedGroup}, nil, ""))
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
	html := renderTo(t, IncidentFeed(1, nil, nil, nil, closed, ""))
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
	html := renderTo(t, IncidentFeed(projectID, nil, items, nil, nil, ""))
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

// TestIncidentFeedHelpPanelAndBackLink — W16/W28: страница объясняет себя
// (helpPanel со ссылкой на /docs/incident-groups) и ведёт обратно на
// страницу аптайм-инцидентов (/projects/{id}/incidents), а не только
// наоборот.
func TestIncidentFeedHelpPanelAndBackLink(t *testing.T) {
	const projectID = int64(5)
	html := renderTo(t, IncidentFeed(projectID, nil, nil, nil, nil, ""))
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
	if !strings.Contains(html, "Инциденты</a>") {
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
	html := renderTo(t, IncidentFeed(1, []GroupCard{group}, outOfGroup, nil, closed, ""))

	wantHeaders := []string{"Источник", "Название", "Статус", "Начало"}
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
	html := renderTo(t, IncidentFeed(1, []GroupCard{group}, outOfGroup, nil, closed, ""))

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
func TestIncidentFeedEmptyStatesUseEmptyStateComponent(t *testing.T) {
	html := renderTo(t, IncidentFeed(1, nil, nil, nil, nil, ""))
	if got := strings.Count(html, `class="empty-state"`); got != 3 {
		t.Errorf("all 3 sections must render @emptyState when empty: got %d, want 3: %s", got, html)
	}
	if got := strings.Count(html, `href="#i-activity"`); got != 3 {
		t.Errorf("empty states must use the activity icon: got %d uses, want 3: %s", got, html)
	}
	// Единственный оставшийся <p class="hint"> — ссылка назад на /incidents
	// под <h1> (не относится к пустым состояниям); если бы эмпти-стейты
	// откатились на старый голый абзац, счёт вырос бы до 4.
	if got := strings.Count(html, `<p class="hint">`); got != 1 {
		t.Errorf("only the back-link hint paragraph should remain, empty sections must use @emptyState: got %d <p class=\"hint\"> blocks, want 1: %s", got, html)
	}
}
