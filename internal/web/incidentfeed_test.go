package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// incidentFeedStack — Handler + Store поверх мигрированной PG, без
// ClickHouse (лента D3 его не читает) — облегчённая версия newHostsStack
// (hosts_web_test.go) для incident-feed.
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

func incidentFeedPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/incident-feed"
}

func TestIncidentFeedEmptyState(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{
		"Лента инцидентов",
		"Открытых групп нет — связанные сбои не обнаружены.",
		"Открытых инцидентов вне групп нет.",
		"За последние сутки ничего не решалось.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("пустая лента не содержит %q: %s", want, text)
		}
	}
}

func TestIncidentFeedGroupWithComposition(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{
		"Хост недоступен",
		rootHostName,
		"host 1 · uptime 1 · metric 0 · slo 0",
		"подавлен: родитель недоступен",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("карточка группы не содержит %q: %s", want, text)
		}
	}
}

func TestIncidentFeedAccessDenied(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

func TestIncidentFeedNilStore(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("h.IncidentGroups == nil: status = %d, want 404", resp.StatusCode)
	}
}

// TestIncidentFeedInvalidProjectID — {id} в пути не парсится как int64:
// parsePathProjectID отдаёт 404 тем же путём, что и остальные ручки проекта
// (projsettings.go), а не 500/панику на мусорном сегменте URL. Тело страницы
// проверяем на ОДНОКРАТНОЕ вхождение текста 404 — если бы ручка не
// прервалась сразу после parsePathProjectID (return по !ok), выполнение
// продолжилось бы с нулевым projectID и дошло бы до собственного notFound
// ручки ещё раз: тот же статус 404 замаскировал бы пропавший return, а
// задвоенное тело — нет.
func TestIncidentFeedInvalidProjectID(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	_, ownerCookie := orgSettingsRegister(t, s.auth, "feed-badid@example.com")

	resp := getWithCookie(t, s.srv, "/projects/not-a-number/incident-feed", ownerCookie)
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

// TestIncidentFeedCanAccessProjectQueryError — сбой самого запроса
// CanAccessProject (не «доступа нет», а поломка БД) обязан отдать 500, а не
// молча отрендерить 404 (это была бы неразличимая с «нет доступа» тишина —
// ровно то, чего инженерные правила требуют избегать). Ломаем role — из неё
// целиком строится org_members-ветка accessCondition (см. org/project.go),
// БД для теста изолирована (testenv.MigratedPG(t) выдаёт свою t_<hash> на
// тест), так что ALTER TABLE не аукается соседям — тот же приём, что и
// TestProfileDeleteLogsWhenEmailReadFails (cover_profile_delete_test.go).
func TestIncidentFeedCanAccessProjectQueryError(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("CanAccessProject query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}

// TestIncidentFeedOpenGroupsQueryError — сбой OpenGroups (queryGroupRows)
// обязан отдать 500. Ломаем root_node_kind — колонку groupRowSelect'а
// (group.go), которую feedMemberSelect/feedProjectQuery (Composition,
// OpenOutOfGroup, ClosedSince) не используют вовсе — обломка бьёт ровно по
// OpenGroups/ClosedGroupsSince, ничего больше в ручке задеть не может.
func TestIncidentFeedOpenGroupsQueryError(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("OpenGroups query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}

// TestIncidentFeedCompositionQueryError — сбой Composition (состав ОДНОЙ из
// открытых/закрытых групп) обязан отдать 500. Заводим ровно одну открытую
// группу — иначе цикл по группам в incidentFeed вообще не вызовет
// Composition. Ломаем ТИП host_incidents.group_id (bigint -> text): его
// сравнивает С ПАРАМЕТРОМ ($1 = group ID) только feedMemberSelect
// (group.go, WHERE hi.group_id = $1) — feedProjectQuery (OpenOutOfGroup/
// ClosedSince) тот же столбец только проверяет на IS NULL, чему тип
// безразличен, так что смена типа рвёт РОВНО Composition и никого из
// соседей (обычный ALTER COLUMN … RENAME сломал бы саму SELECT-колонку и
// задел бы оба запроса — здесь нужна асимметрия именно по сравнению типов).
func TestIncidentFeedCompositionQueryError(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Composition query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}

// TestIncidentFeedOpenOutOfGroupQueryError — сбой OpenOutOfGroup (6-источник-
// ный feedProjectQuery) обязан отдать 500. Групп в проекте нет (иначе
// Composition сработал бы раньше и замаскировал бы именно эту ветку), ломаем
// perf_regressions.metric — колонку trace-ветки feedProjectQuery, которую ни
// groupRowSelect, ни feedMemberSelect не используют.
func TestIncidentFeedOpenOutOfGroupQueryError(t *testing.T) {
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

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("OpenOutOfGroup query error: status = %d, want 500: %s", resp.StatusCode, body)
	}
}
