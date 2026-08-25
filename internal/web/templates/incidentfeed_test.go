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
	if strings.Contains(html, "<tr>") {
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
	if strings.Contains(plain, "подавлен: родитель недоступен") || strings.Contains(plain, "подтверждён") {
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
	if strings.Contains(suppressedHTML, "подтверждён") {
		t.Errorf("suppressed-only row must not show the ack badge: %s", suppressedHTML)
	}

	acked := base
	acked.Acknowledged = true
	ackedHTML := renderTo(t, feedItemRow(1, acked))
	if !strings.Contains(ackedHTML, "подтверждён") {
		t.Errorf("acked row must show the ack badge: %s", ackedHTML)
	}
	if strings.Contains(ackedHTML, "подавлен: родитель недоступен") {
		t.Errorf("acked-only row must not show the suppression badge: %s", ackedHTML)
	}

	both := base
	both.SuppressedByDep = true
	both.Acknowledged = true
	bothHTML := renderTo(t, feedItemRow(1, both))
	if !strings.Contains(bothHTML, "подавлен: родитель недоступен") || !strings.Contains(bothHTML, "подтверждён") {
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
	if !strings.Contains(html, "Открытых инцидентов вне групп нет.") {
		t.Errorf("out-of-group section must still show its empty state: %s", html)
	}
	if !strings.Contains(html, "За последние сутки ничего не решалось.") {
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
	if strings.Contains(html, "Открытых инцидентов вне групп нет.") {
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
	if strings.Contains(html, "За последние сутки ничего не решалось.") {
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
	if strings.Contains(html, "За последние сутки ничего не решалось.") {
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
