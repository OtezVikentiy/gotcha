package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// incidentFeedStack — Handler + Store поверх мигрированной PG, без
// ClickHouse (шкала «Обзора» его не читает) — облегчённая версия
// newHostsStack (hosts_web_test.go) для overview.
type incidentFeedStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	h      *web.Handler
	org    *org.Service
	auth   *auth.Service
	groups *incidentgroup.Store
}

// newIncidentFeedStack — wire=false воспроизводит стенд без подсистемы
// корреляции (h.IncidentGroups остаётся nil), как newHostsStack(t, false)
// для Metrics/Hosts.
func newIncidentFeedStack(t *testing.T, wire bool) *incidentFeedStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	groups := incidentgroup.NewStore(pool)
	if wire {
		h.IncidentGroups = groups
	}
	h.Register(mux)
	return &incidentFeedStack{pool: pool, srv: srv, h: h, org: orgSvc, auth: authSvc, groups: groups}
}

// seedFeedHost — минимальная строка hosts (как seedHost в
// internal/incidentgroup/group_test.go, недоступном отсюда — другой пакет).
func (s *incidentFeedStack) seedFeedHost(t *testing.T, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,'','') RETURNING id`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed host %s: %v", name, err)
	}
	return id
}

// seedFeedHostIncident — открытый host_incidents (kind/detail минимальны).
func (s *incidentFeedStack) seedFeedHostIncident(t *testing.T, projectID, hostID int64, kind string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(), `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,$3,'open',0,0,'') RETURNING id`,
		projectID, hostID, kind).Scan(&id); err != nil {
		t.Fatalf("seed host incident: %v", err)
	}
	return id
}

func overviewPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/overview"
}

// TestOverviewEmptyProject — проект без единого инцидента получает не три
// «нет данных» подряд, а одно приглашение подключить SDK (задача 6 nav-ia,
// §7 спеки — «пустое состояние — приглашение к следующему шагу, а не «нет
// данных»»).
func TestOverviewEmptyProject(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co", "Feed Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj", "Feed Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, "Обзор пока пуст") {
		t.Errorf("empty overview must invite the next onboarding step, not render per-section empty states: %s", text)
	}
	if !strings.Contains(text, `href="`+"/projects/"+strconv.FormatInt(project.ID, 10)+"/setup"+`"`) {
		t.Errorf("empty overview must link to the project's getting-started page: %s", text)
	}
	if strings.Contains(text, "Открытых групп нет") {
		t.Errorf("a totally empty overview must not ALSO render the old per-section empty states: %s", text)
	}
}

func TestOverviewGroupWithComposition(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner2@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co2", "Feed Co 2", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj2", "Feed Proj 2", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	rootHostName := "root-gw"
	rootHost := s.seedFeedHost(t, project.ID, rootHostName)
	var rootInc int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',true) RETURNING id`, project.ID, rootHost).Scan(&rootInc); err != nil {
		t.Fatalf("seed root incident: %v", err)
	}
	group, err := s.groups.EnsureGroup(ctx, project.ID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	memberHost := s.seedFeedHost(t, project.ID, "member-web")
	memberInc := s.seedFeedHostIncident(t, project.ID, memberHost, "disk")
	if _, err := s.groups.SetGroup(ctx, project.ID, "host", memberInc, group.ID); err != nil {
		t.Fatalf("SetGroup host: %v", err)
	}

	var monitorID, uptimeMemberInc int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'api-mon','http',60) RETURNING id`, project.ID).Scan(&monitorID); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO incidents (monitor_id, notified_open, suppressed_by_dep)
		VALUES ($1,true,true) RETURNING id`, monitorID).Scan(&uptimeMemberInc); err != nil {
		t.Fatalf("seed uptime incident: %v", err)
	}
	if _, err := s.groups.SetGroup(ctx, project.ID, "uptime", uptimeMemberInc, group.ID); err != nil {
		t.Fatalf("SetGroup uptime: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{
		"Хост недоступен",
		rootHostName,
		"всего 2 (Хост 1 · Аптайм 1)",
		"подавлен: родитель недоступен",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("карточка группы не содержит %q: %s", want, text)
		}
	}
}

func TestOverviewAccessDenied(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, _ := orgSettingsRegister(t, s.auth, "feed-owner3@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co3", "Feed Co 3", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj3", "Feed Proj 3", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "feed-outsider@example.com")

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

// TestOverviewNilStoreRendersEmpty — задача 6 nav-ia: в отличие от прежней
// /incident-feed (404 на h.IncidentGroups == nil, «стенд без подсистемы»),
// «Обзор» — теперь дверь по умолчанию (index() ведёт сюда), и 404 на голом
// входе в приложение читался бы как поломка. Инстанс/стенд без подсистемы
// D3 получает страницу с пустыми секциями (то же приглашение, что и у
// TestOverviewEmptyProject), а не ошибку.
func TestOverviewNilStoreRendersEmpty(t *testing.T) {
	s := newIncidentFeedStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner4@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co4", "Feed Co 4", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj4", "Feed Proj 4", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("h.IncidentGroups == nil: status = %d, want 200 (overview renders, doesn't 404): %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Обзор пока пуст") {
		t.Errorf("overview without the IncidentGroups store must still render the empty-project invite: %s", body)
	}
}

// TestOverviewInvalidProjectID — {id} в пути не парсится как int64:
// parsePathProjectID отдаёт 404 тем же путём, что и остальные ручки проекта
// (projsettings.go), а не 500/панику на мусорном сегменте URL. Тело страницы
// проверяем на ОДНОКРАТНОЕ вхождение текста 404 — если бы ручка не
// прервалась сразу после parsePathProjectID (return по !ok), выполнение
// продолжилось бы с нулевым projectID и дошло бы до собственного notFound
// ручки ещё раз: тот же статус 404 замаскировал бы пропавший return, а
// задвоенное тело — нет.
func TestOverviewInvalidProjectID(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	_, ownerCookie := orgSettingsRegister(t, s.auth, "feed-badid@example.com")

	resp := getWithCookie(t, s.srv, "/projects/not-a-number/overview", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid project id in path: status = %d, want 404: %s", resp.StatusCode, body)
	}
	// ErrorPage сама повторяет текст дважды (заголовок h1 + тело p) — один
	// рендер даёт 2 вхождения; удвоение (return пропал) даёт 4.
	if n := strings.Count(string(body), "Страница не найдена"); n != 2 {
		t.Fatalf("404 body must render exactly once (return after parsePathProjectID failure), got %d occurrences of the message (want 2, one render): %s", n, body)
	}
}

// TestOverviewCanAccessProjectQueryError — сбой самого запроса
// CanAccessProject (не «доступа нет», а поломка БД) обязан отдать 500, а не
// молча отрендерить 404 (это была бы неразличимая с «нет доступа» тишина —
// ровно то, чего инженерные правила требуют избегать). Ломаем role — из неё
// целиком строится org_members-ветка accessCondition (см. org/project.go),
// БД для теста изолирована (testenv.MigratedPG(t) выдаёт свою t_<hash> на
// тест), так что ALTER TABLE не аукается соседям — тот же приём, что и
// TestProfileDeleteLogsWhenEmailReadFails (cover_profile_delete_test.go).
func TestOverviewCanAccessProjectQueryError(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-accesserr@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co-accesserr", "Feed Co AccessErr", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj-accesserr", "Feed Proj AccessErr", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if _, err := s.pool.Exec(ctx, "ALTER TABLE org_members RENAME COLUMN role TO role_broken_for_test"); err != nil {
		t.Fatalf("break org_members.role: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("CanAccessProject query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}

// TestOverviewOpenGroupsQueryError — сбой OpenGroups (queryGroupRows)
// обязан отдать 500. Ломаем root_node_kind — колонку groupRowSelect'а
// (group.go), которую feedMemberSelect/feedProjectQuery (Composition,
// OpenOutOfGroup, ClosedSince) не используют вовсе — обломка бьёт ровно по
// OpenGroups/ClosedGroupsSince, ничего больше в ручке задеть не может.
func TestOverviewOpenGroupsQueryError(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-opengroupserr@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co-opengroupserr", "Feed Co OpenGroupsErr", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj-opengroupserr", "Feed Proj OpenGroupsErr", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if _, err := s.pool.Exec(ctx,
		"ALTER TABLE incident_groups RENAME COLUMN root_node_kind TO root_node_kind_broken_for_test"); err != nil {
		t.Fatalf("break incident_groups.root_node_kind: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("OpenGroups query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}

// TestOverviewCompositionQueryError — сбой Compositions (состав ВСЕХ
// открытых/закрытых групп разом, один батч-запрос, W7) обязан отдать 500.
// Заводим ровно одну открытую группу — иначе overview вообще не собрал бы
// непустой groupIDs и не позвал бы Compositions. Ломаем ТИП
// host_incidents.group_id (bigint -> text): его сравнивает С ПАРАМЕТРОМ
// ($1 = ANY(group IDs)) только feedMemberSelectBatch (group.go, WHERE
// hi.group_id = ANY($1)) — feedProjectQuery (OpenOutOfGroup/ClosedSince)
// тот же столбец только проверяет на IS NULL, чему тип безразличен, так что
// смена типа рвёт РОВНО Compositions и никого из соседей (обычный
// ALTER COLUMN … RENAME сломал бы саму SELECT-колонку и задел бы оба
// запроса — здесь нужна асимметрия именно по сравнению типов).
func TestOverviewCompositionQueryError(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-compositionerr@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co-compositionerr", "Feed Co CompositionErr", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj-compositionerr", "Feed Proj CompositionErr", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	rootHost := s.seedFeedHost(t, project.ID, "root-compositionerr")
	var rootInc int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',true) RETURNING id`, project.ID, rootHost).Scan(&rootInc); err != nil {
		t.Fatalf("seed root incident: %v", err)
	}
	if _, err := s.groups.EnsureGroup(ctx, project.ID, "host", rootInc, "host", rootHost); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	if _, err := s.pool.Exec(ctx,
		"ALTER TABLE host_incidents ALTER COLUMN group_id TYPE text"); err != nil {
		t.Fatalf("break host_incidents.group_id type: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Composition query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}

// TestOverviewOpenOutOfGroupQueryError — сбой OpenOutOfGroup (6-источник-
// ный feedProjectQuery) обязан отдать 500. Групп в проекте нет (иначе
// Composition сработал бы раньше и замаскировал бы именно эту ветку), ломаем
// perf_regressions.metric — колонку trace-ветки feedProjectQuery, которую ни
// groupRowSelect, ни feedMemberSelect не используют.
func TestOverviewOpenOutOfGroupQueryError(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-outofgrouperr@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co-outofgrouperr", "Feed Co OutOfGroupErr", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj-outofgrouperr", "Feed Proj OutOfGroupErr", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if _, err := s.pool.Exec(ctx,
		"ALTER TABLE perf_regressions RENAME COLUMN metric TO metric_broken_for_test"); err != nil {
		t.Fatalf("break perf_regressions.metric: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("OpenOutOfGroup query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}

// TestOverviewMultipleGroupsCompositionNotCrossed — W7: overview собирает
// состав ВСЕХ карточек одним батч-запросом (h.IncidentGroups.Compositions)
// вместо цикла Composition-по-группе. Главный риск батча — перепутать
// member'ов между группами через общий список groupIDs/map (см. ту же
// тревогу в feed_test.go: TestFeedCompositionsBatch на уровне стора). Здесь
// та же проверка на уровне HTTP-ответа: две открытые группы с РАЗНЫМИ по
// имени членами, каждая карточка обязана показать ТОЛЬКО своего.
func TestOverviewMultipleGroupsCompositionNotCrossed(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner5@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co5", "Feed Co 5", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj5", "Feed Proj 5", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	makeGroup := func(rootName, memberName string) {
		rootHost := s.seedFeedHost(t, project.ID, rootName)
		var rootInc int64
		if err := s.pool.QueryRow(ctx, `
			INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
			VALUES ($1,$2,'silent','open',0,0,'',true) RETURNING id`, project.ID, rootHost).Scan(&rootInc); err != nil {
			t.Fatalf("seed root incident %s: %v", rootName, err)
		}
		g, err := s.groups.EnsureGroup(ctx, project.ID, "host", rootInc, "host", rootHost)
		if err != nil {
			t.Fatalf("EnsureGroup %s: %v", rootName, err)
		}
		memberHost := s.seedFeedHost(t, project.ID, memberName)
		memberInc := s.seedFeedHostIncident(t, project.ID, memberHost, "disk")
		if _, err := s.groups.SetGroup(ctx, project.ID, "host", memberInc, g.ID); err != nil {
			t.Fatalf("SetGroup %s: %v", rootName, err)
		}
	}
	makeGroup("root-alpha", "member-alpha")
	makeGroup("root-beta", "member-beta")

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)

	alphaIdx := strings.Index(text, "root-alpha")
	betaIdx := strings.Index(text, "root-beta")
	if alphaIdx < 0 || betaIdx < 0 {
		t.Fatalf("both group root names must appear: %s", text)
	}
	// Каждая карточка — от своего заголовка до начала следующей (или конца
	// секции) — обязана содержать СВОЕГО члена и не содержать чужого.
	var alphaCard, betaCard string
	if alphaIdx < betaIdx {
		alphaCard, betaCard = text[alphaIdx:betaIdx], text[betaIdx:]
	} else {
		betaCard, alphaCard = text[betaIdx:alphaIdx], text[alphaIdx:]
	}
	if !strings.Contains(alphaCard, "member-alpha") {
		t.Errorf("root-alpha card must contain its own member: %s", alphaCard)
	}
	if strings.Contains(alphaCard, "member-beta") {
		t.Errorf("root-alpha card must NOT contain the other group's member: %s", alphaCard)
	}
	if !strings.Contains(betaCard, "member-beta") {
		t.Errorf("root-beta card must contain its own member: %s", betaCard)
	}
}

// TestOverviewCapCaptions — W7/W8: подписи потолков рядом с заголовками
// секций — реальные числа (incidentgroup.MaxOpenGroups/MaxOpenOutOfGroup,
// overviewClosedGroupsLimit/overviewClosedOutOfGroupLimit), а не нули
// FeedCaps{} и не единое (уже неверное) число на обе закрытые секции разом
// (см. докблок overviewClosedGroupsLimit/overviewClosedOutOfGroupLimit).
// Заводим одну открытую группу — иначе рендер ушёл бы в ветку «проект
// совсем пуст» (задача 6 nav-ia) и заголовки секций не появились бы вовсе.
func TestOverviewCapCaptions(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner6@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co6", "Feed Co 6", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj6", "Feed Proj 6", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	rootHost := s.seedFeedHost(t, project.ID, "root-cap")
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',true)`, project.ID, rootHost); err != nil {
		t.Fatalf("seed root incident: %v", err)
	}

	resp := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{
		"не больше 50",
		"за последние 24 ч: групп не больше 50, отдельных инцидентов не больше 50",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("overview page missing cap caption %q: %s", want, text)
		}
	}
}

// TestOverviewRangeToggleSelectsWindow — переключатель ?range= секции
// «недавно решённые» (задача 6 nav-ia): 24ч — умолчание, канонический адрес
// без параметра; 7д — явный ?range=7d, отражается и в подписи окна, и в
// активной вкладке; нераспознанное значение откатывается на 24ч, а не падает.
// Заводим одну открытую группу — иначе рендер ушёл бы в ветку «проект
// совсем пуст», где ни подписи, ни вкладок вовсе нет.
func TestOverviewRangeToggleSelectsWindow(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-range@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co-range", "Feed Co Range", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj-range", "Feed Proj Range", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	rootHost := s.seedFeedHost(t, project.ID, "root-range")
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',true)`, project.ID, rootHost); err != nil {
		t.Fatalf("seed root incident: %v", err)
	}

	cases := []struct {
		suffix     string
		wantWindow string
		wantActive string
	}{
		{"", "за последние 24 ч", `href="` + overviewPath(project.ID) + `" aria-current="page"`},
		{"?range=7d", "за последние 7 дн", `href="` + overviewPath(project.ID) + `?range=7d" aria-current="page"`},
		{"?range=bogus", "за последние 24 ч", `href="` + overviewPath(project.ID) + `" aria-current="page"`},
	}
	for _, c := range cases {
		resp := getWithCookie(t, s.srv, overviewPath(project.ID)+c.suffix, ownerCookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("range=%q: status = %d, want 200: %s", c.suffix, resp.StatusCode, body)
		}
		text := string(body)
		if !strings.Contains(text, c.wantWindow) {
			t.Errorf("range=%q: missing window caption %q: %s", c.suffix, c.wantWindow, text)
		}
		if !strings.Contains(text, c.wantActive) {
			t.Errorf("range=%q: missing active tab %q: %s", c.suffix, c.wantActive, text)
		}
	}
}

// TestOverviewRangeWidensClosedWindowFiltering — фикс-раунд 1 (ревью
// задачи 6): TestOverviewRangeToggleSelectsWindow проверяет только подпись
// окна и активную вкладку — обе выводятся из строки rangeKey, не из
// данных, и не заметили бы, если бы since перестал зависеть от диапазона
// (ревьюер воспроизвёл это, зашив since на фиксированные 24ч — весь набор
// TestOverview* остался зелёным). Здесь — фактическая фильтрация по
// границе окна: инцидент, закрытый 3 суток назад (старше 24ч, моложе 7д),
// отсутствует в выдаче на умолчании (?range пусто/24h) и появляется на
// ?range=7d.
func TestOverviewRangeWidensClosedWindowFiltering(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-window@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co-window", "Feed Co Window", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj-window", "Feed Proj Window", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	hostName := "closed-3d-ago"
	host := s.seedFeedHost(t, project.ID, hostName)
	resolvedAt := time.Now().Add(-72 * time.Hour)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, started_at, resolved_at)
		VALUES ($1,$2,'silent','resolved',0,0,'',$3,$3)`,
		project.ID, host, resolvedAt); err != nil {
		t.Fatalf("seed closed host incident: %v", err)
	}

	resp24 := getWithCookie(t, s.srv, overviewPath(project.ID), ownerCookie)
	body24, _ := io.ReadAll(resp24.Body)
	resp24.Body.Close()
	if resp24.StatusCode != http.StatusOK {
		t.Fatalf("range=24h (default): status = %d, want 200: %s", resp24.StatusCode, body24)
	}
	if strings.Contains(string(body24), hostName) {
		t.Errorf("range=24h (default): an incident closed 3 days ago must NOT appear in a 24h window: %s", body24)
	}

	resp7d := getWithCookie(t, s.srv, overviewPath(project.ID)+"?range=7d", ownerCookie)
	body7d, _ := io.ReadAll(resp7d.Body)
	resp7d.Body.Close()
	if resp7d.StatusCode != http.StatusOK {
		t.Fatalf("range=7d: status = %d, want 200: %s", resp7d.StatusCode, body7d)
	}
	if !strings.Contains(string(body7d), hostName) {
		t.Errorf("range=7d: an incident closed 3 days ago must appear once the window widens to 7 days: %s", body7d)
	}
}

// TestIncidentFeedRedirectsToOverview — старый адрес ленты (D3) целиком
// редиректит на «Обзор» (задача 6 nav-ia): экран не должен остаться
// достижимым по двум разным путям.
func TestIncidentFeedRedirectsToOverview(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "feed@example.com")
	p := createProject(t, s, uid, "feed-org", "feed-proj")
	pid := strconv.FormatInt(p.ID, 10)

	resp := getWithCookie(t, s.srv, "/projects/"+pid+"/incident-feed", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/projects/"+pid+"/overview") {
		t.Fatalf("Location = %q, want /projects/%s/overview", loc, pid)
	}
}

// TestIncidentFeedRedirectInvalidProjectID — редирект со старого адреса
// ленты, как и сама /overview, отдаёт 404 на нечисловой {id}, а не 500/пустой
// Location (несуществующий числовой id тоже безопасен: редирект слепой,
// доступ проверяет цель редиректа — overview).
func TestIncidentFeedRedirectInvalidProjectID(t *testing.T) {
	s := newIssuesStack(t)
	_, cookie := registerAndLogin(t, s, "feed-badid@example.com")

	resp := getWithCookie(t, s.srv, "/projects/not-a-number/incident-feed", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestCookieOnlyDecidesTheDoor — правило состояния (§5 спеки): кука имеет
// право голоса только на голом /, все остальные экраны берут истину из URL.
// Без этого теста через полгода правило «оптимизируют»: две вкладки с
// разными проектами начнут перебивать друг друга.
func TestCookieOnlyDecidesTheDoor(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "tabs@example.com")
	a := createProject(t, s, uid, "org-a", "proj-a")
	b := createProject(t, s, uid, "org-b", "proj-b")

	// заходим в проект A — кука запоминает его
	respA := getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(a.ID, 10)+"/overview", cookie)
	respA.Body.Close()

	// прямой адрес проекта B обязан открыть B, а не запомненный A:
	// иначе две вкладки с разными проектами перебивают друг друга
	respB := getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(b.ID, 10)+"/overview", cookie)
	defer respB.Body.Close()
	body, _ := io.ReadAll(respB.Body)
	if !strings.Contains(string(body), b.Name) || strings.Contains(string(body), a.Name) {
		t.Fatal("экран смотрит в куку вместо URL: работа в нескольких вкладках сломана")
	}
}

// index()'s cookie-remembers-project / falls-back-to-org-projects behavior
// (задача 6 nav-ia, шаг 5) is covered by TestIndexStickyProject
// (projcookie_test.go) — the authoritative test for that logic, updated in
// this same task; no separate copy here to avoid duplicate, weaker coverage
// of the same rule.

// TestOverviewStatusLineIsClickable — строка состояния (задача 7 nav-ia):
// три числа над шкалой инцидентов, каждое ссылкой в свой раздел (аптайм →
// мониторы, хосты за порогом → хосты, новые проблемы → issues). Проверяем
// только наличие живых ссылок, не значения самих чисел — те завязаны на
// данные, которых в свежесозданном проекте нет ни по одному источнику
// (h.Uptime/h.HostIncidents/h.Deploy остаются nil в newIssuesStack), а
// строка обязана рендериться и в этом состоянии (без данных — не значит без
// ссылок).
func TestOverviewStatusLineIsClickable(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "status@example.com")
	p := createProject(t, s, uid, "st-org", "st-proj")
	pid := strconv.FormatInt(p.ID, 10)

	resp := getWithCookie(t, s.srv, "/projects/"+pid+"/overview", cookie)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, href := range []string{"/monitors", "/hosts", "/issues"} {
		if !strings.Contains(string(body), "/projects/"+pid+href) {
			t.Errorf("строка состояния не ведёт в %s: мёртвых чисел на обзоре быть не должно", href)
		}
	}
}

// TestOverviewShowsDeployMarkers — деплои (C5) на той же временной оси, что
// и инциденты (задача 7 nav-ia): деплой внутри окна обзора должен быть
// виден на странице, чтобы отвечать на вопрос «после выкатки или само». В
// отличие от исходной версии теста (ревью фикс-раунда 1: ревьюер снял окно
// целиком — `since, now` → `time.Time{}, now.Add(999*time.Hour)` — и весь
// TestOverview* остался зелёным, потому что отсутствие деплоя ВНЕ окна не
// проверял никто), здесь второй деплой заведён ЗА пределами окна 24ч (но
// внутри 7д) и должен ОТСУТСТВОВАТЬ на дефолтном (24ч) экране, а при явном
// расширении окна (?range=7d, тот же переключатель, что и у «недавно
// решённых») — появиться.
func TestOverviewShowsDeployMarkers(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "deploy@example.com")
	p := createProject(t, s, uid, "dep-org", "dep-proj")
	pid := strconv.FormatInt(p.ID, 10)
	// Деплои внутри/вне окна обзора. Отдельного помощника вставки в пакете
	// нет: TestWebDeploymentsScreen (internal/web/deployments_test.go:54)
	// пишет деплой в БД прямо в теле теста — повторить оттуда те же вызовы,
	// подставив нужное время. newIssuesStack не заводит h.Deploy (стенд
	// задачи 4 issues его не знает) — заводим сами через тот же
	// deploy.NewStore(s.pool), что и newDeployStack.
	depSvc := deploy.NewStore(s.pool)
	s.h.Deploy = depSvc

	const insideVersion = "v1.2.3-overview"
	if _, err := depSvc.Record(context.Background(), p.ID, deploy.Deployment{
		Version:     insideVersion,
		Environment: "prod",
		DeployedAt:  time.Now().UTC().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("record inside-window deploy: %v", err)
	}
	// -25h: вне окна 24ч по умолчанию, но внутри окна 7д (rangeKey ниже).
	const outsideVersion = "v0.9.0-before-window"
	if _, err := depSvc.Record(context.Background(), p.ID, deploy.Deployment{
		Version:     outsideVersion,
		Environment: "prod",
		DeployedAt:  time.Now().UTC().Add(-25 * time.Hour),
	}); err != nil {
		t.Fatalf("record outside-window deploy: %v", err)
	}

	resp := getWithCookie(t, s.srv, "/projects/"+pid+"/overview", cookie)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, insideVersion) {
		t.Fatalf("маркер деплоя %q не попал на шкалу (окно 24ч по умолчанию): %s", insideVersion, text)
	}
	if strings.Contains(text, outsideVersion) {
		t.Fatalf("маркер деплоя %q старше окна 24ч не должен рисоваться по умолчанию: %s", outsideVersion, text)
	}

	respWide := getWithCookie(t, s.srv, "/projects/"+pid+"/overview?range=7d", cookie)
	defer respWide.Body.Close()
	bodyWide, _ := io.ReadAll(respWide.Body)
	textWide := string(bodyWide)
	if !strings.Contains(textWide, outsideVersion) {
		t.Fatalf("маркер деплоя %q обязан появиться при расширении окна до 7д: %s", outsideVersion, textWide)
	}
	if !strings.Contains(textWide, insideVersion) {
		t.Fatalf("расширение окна до 7д не должно терять более свежий деплой %q: %s", insideVersion, textWide)
	}
}
