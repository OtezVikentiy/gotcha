package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/guards"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// B3 (arch P1-4 / security P2-4 / qa P2-3): карта прав (authz_map_test.go)
// декларирует уровень доступа маршрута, но ничем не проверяет, что ХЕНДЛЕР
// действительно навешивает соответствующий гейт — обработчик, тихо
// переписанный с requireProjectOperator на requireProjectRole (или вовсе
// потерявший гейт), карту не потревожит, а тест на конкретный сценарий может
// его не заметить, если тестируется не тот путь. Этот файл ловит именно
// разрыв «декларация → применение» одной дешёвой персоной: чужак — участник
// ДРУГОЙ организации, с нулевым членством в организации жертвы, — не должен
// пройти НИ ОДИН project/org-scoped маршрут.
//
// Дёшево это ровно потому, что аудит подтвердил: во всех гейтящих хендлерах
// этой правки проверка прав стоит ДО r.ParseForm — значит, пустого POST-тела
// достаточно, чтобы дойти до гейта и получить отказ, не заботясь о том, какие
// поля формы нужны конкретному маршруту.

// strangerScopedPrefixes — префиксы шаблонов маршрутов, адресующих ресурс по
// id в пути (проект/организация/монитор/статус-страница/команда/issue/
// perf-issue). Ими и только ими ограничен перебор: остальные маршруты либо
// публичные (login/heartbeat/probe/status), либо lvlUser без адресации по
// чужому ресурсу (профиль, онбординг, /projects список) — чужаку там нечего
// красть, потому что там нет ЧУЖОГО id. Список независим от authz_map_test.go
// и от карты уровней B2 (которая может её расширять параллельно) — оба фикса
// работают над одним web.go, не деля общий символ.
var strangerScopedPrefixes = []string{
	"/projects/{id}",
	"/orgs/{id}",
	"/monitors/{id}",
	"/statuspages/{id}",
	"/teams/{id}",
	"/perf-issues/{id}",
	"/issues/{id}",
}

func isStrangerScopedRoute(path string) bool {
	for _, p := range strangerScopedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// leaveConfirmExemption — POST /orgs/{id}/settings/leave: самостоятельный
// выход из организации по дизайну не несёт гейта роли (см. lvlUser в
// authz_map_test.go — RemoveMember(orgID, uid) сам проверяет членство).
// Без confirmed=yes хендлер, ДО какой бы то ни было проверки членства,
// показывает экран подтверждения (200) — то же самое renderConfirm видит и
// owner, и полный чужак (orgSettingsLeave.go: parsePathOrgID даже не ходит в
// БД, confirm.org_leave.message — статическая строка без имени/slug
// организации, см. i18n/locales/*.json). Это не разрыв декларация↔применение:
// РЕАЛЬНАЯ мутация (confirmed=yes) по-прежнему упирается в
// ErrNotMember → 404 — это отдельно проверяется ниже, за пределами общего
// цикла. Единственное исключение из «первый POST обязан получить 404/403» в
// этом файле — потолок 1, теми же приёмами, что и originExemptions
// (routes_origin_test.go).
var leaveConfirmExemption = []guards.Exemption{
	{
		Value:   "POST /orgs/{id}/settings/leave",
		Why:     "экран подтверждения общий для всех и не палит организацию; настоящая мутация (confirmed=yes) отдельно проверена на 404 ниже",
		Finding: "B3",
	},
}

// victimIDs — реальные id ресурсов организации-жертвы, живущих в стенде:
// подставляются вместо {id} в зависимости от того, какой тип ресурса адресует
// конкретный маршрут (определяется по префиксу, см. strangerScopedPrefixes).
type victimIDs struct {
	orgID        int64
	projectID    int64
	monitorID    int64
	statuspageID int64
	teamID       int64
	issueID      int64
	perfIssueID  int64
}

// concreteVictimPath подставляет в шаблон маршрута реальный id ресурса
// жертвы того типа, которым адресован маршрут (см. strangerScopedPrefixes —
// префиксы там же и там же не пересекаются, порядок проверки не важен).
// Любые ДРУГИЕ плейсхолдеры в пути (например {name} в /metrics/{name} или
// {transaction...}) не несут авторизационного смысла — это данные ВНУТРИ уже
// проверенного проекта, не отдельные защищаемые ресурсы, поэтому их
// достаточно заменить безобидной строкой той же функцией, что использует
// сторож Origin (routePlaceholder, routes_origin_test.go).
func concreteVictimPath(t *testing.T, path string, v victimIDs) string {
	t.Helper()
	switch {
	case strings.HasPrefix(path, "/projects/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.projectID, 10), 1)
	case strings.HasPrefix(path, "/orgs/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.orgID, 10), 1)
	case strings.HasPrefix(path, "/monitors/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.monitorID, 10), 1)
	case strings.HasPrefix(path, "/statuspages/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.statuspageID, 10), 1)
	case strings.HasPrefix(path, "/teams/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.teamID, 10), 1)
	case strings.HasPrefix(path, "/perf-issues/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.perfIssueID, 10), 1)
	case strings.HasPrefix(path, "/issues/{id}"):
		path = strings.Replace(path, "{id}", strconv.FormatInt(v.issueID, 10), 1)
	default:
		t.Fatalf("маршрут %q не подпадает ни под один известный тип ресурса — обнови concreteVictimPath/strangerScopedPrefixes", path)
	}
	return routePlaceholder.ReplaceAllString(path, "x")
}

// TestAuthzBehaviorStrangerRejectedOnScopedRoutes — поведенческая матрица
// B3: чужак (владелец ДРУГОЙ организации, нулевое членство в организации
// жертвы) шлёт запрос на КАЖДЫЙ project/org-scoped маршрут стенда (GET как
// есть, POST с пустым телом и валидным Origin — иначе всё легло бы на
// sameOrigin, а не на проверяемый гейт) и обязан получить 404 или 403. После
// прогона все ресурсы жертвы перечитываются — ни один POST не должен был
// мутировать.
//
// Стенд — newUptimeStack: регистрирует все маршруты безусловно (машинные
// heartbeat/probe — под `if h.Uptime != nil`, но и они попадают в перебор
// как исключение; здесь же они просто не матчат strangerScopedPrefixes и в
// выборку не входят). Одного newUptimeStack мало: сами хендлеры сначала
// проверяют профильный сервис на nil (h.Alerts/h.Trace/h.PerfIssues/...) и
// ТОЛЬКО потом — гейт доступа (см. alertsPage, metricAlertsPage,
// perfIssuesList, performanceList и др.) — без дозаводки этих сервисов
// чужак получал бы 404 от nil-guard'а, ни разу не долетев до самого гейта,
// который и обязан проверить этот тест. Поэтому здесь довооружается тот же
// набор полей, что main.go выставляет в режиме "web"/"all".
func TestAuthzBehaviorStrangerRejectedOnScopedRoutes(t *testing.T) {
	s := newUptimeStack(t)
	ch := testenv.MigratedCH(t)

	s.h.Alerts = alert.NewService(s.pool)
	s.h.Outbox = notify.NewOutbox(s.pool)
	// Пустой Direct: сендеров нет, но POST'у с пустым телом до Send дойти
	// не положено — гейт должен отказать раньше. Нужен только не-nil, чтобы
	// alertsChannelTest не срезал запрос собственным nil-guard'ом до гейта.
	s.h.NotifyDirect = &notify.Direct{}
	s.h.UptimeQuery = uptime.NewQuery(ch)
	s.h.Trace = trace.NewQuery(ch)
	s.h.PerfIssues = trace.NewIssueService(s.pool)
	s.h.Regressions = trace.NewRegressionService(s.pool)
	s.h.Metrics = metric.NewQuery(ch)
	s.h.MetricRules = metric.NewRuleService(s.pool)
	s.h.MetricIncidents = metric.NewIncidentService(s.pool)
	s.h.Profiles = profile.NewQuery(ch)
	s.h.ProfileRegressions = profile.NewRegressionService(s.pool)

	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	// Первый Register на пустом инстансе становится bootstrap
	// instance-admin (PROD-B1, auth.Service.Register) — SSO-ручки гейтятся
	// именно им (requireInstanceAdminForSSO), не членством в организации.
	// Расходуем этот слот на одноразового пользователя ДО чужака: иначе
	// чужак сам стал бы инстанс-админом и легитимно проходил бы SSO-ручки
	// ЛЮБОЙ организации — не разрыв гейта, а особенность порядка регистрации
	// в тесте.
	orgSettingsRegister(t, authSvc, "b3-bootstrap-sink@example.com")

	// Чужак: полноценный залогиненный пользователь, владелец СВОЕЙ
	// организации — но ни единого membership в организации жертвы.
	strangerID, strangerCookie := orgSettingsRegister(t, authSvc, "b3-stranger@example.com")
	if _, err := orgSvc.CreateOrg(ctx, "b3-stranger-co", "B3 Stranger Co", strangerID); err != nil {
		t.Fatalf("create stranger org: %v", err)
	}

	// Организация-жертва с полным набором ресурсов, по одному на каждый тип
	// id, встречающийся в путях (см. strangerScopedPrefixes) — так 404
	// доказывает отказ РЕАЛЬНО СУЩЕСТВУЮЩЕМУ ресурсу, а не просто
	// несуществующему id.
	victimOwnerID, _ := orgSettingsRegister(t, authSvc, "b3-victim@example.com")
	victimOrg, err := orgSvc.CreateOrg(ctx, "b3-victim-co", "B3 Victim Co", victimOwnerID)
	if err != nil {
		t.Fatalf("create victim org: %v", err)
	}
	victimProject, err := orgSvc.CreateProject(ctx, victimOrg.ID, "b3-victim-proj", "B3 Victim Proj", "go")
	if err != nil {
		t.Fatalf("create victim project: %v", err)
	}

	hbCfg, err := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 120})
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}
	victimMonitor, err := s.uptime.Create(ctx, uptime.Monitor{
		ProjectID:         victimProject.ID,
		Name:              "b3 victim monitor",
		Kind:              uptime.KindHeartbeat,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     1,
		RecoveryThreshold: 1,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
		Config:            hbCfg,
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create victim monitor: %v", err)
	}

	victimTeam, err := orgSvc.CreateTeam(ctx, victimOrg.ID, "b3-victim-team", "B3 Victim Team")
	if err != nil {
		t.Fatalf("create victim team: %v", err)
	}

	victimSP, err := s.uptime.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: victimProject.ID,
		Title:     "B3 Victim SP",
		Enabled:   true,
	}, nil)
	if err != nil {
		t.Fatalf("create victim statuspage: %v", err)
	}

	issueRes, err := s.h.Issues.Upsert(ctx, victimProject.ID, "b3-victim-fp", "B3 victim issue", "b3.Culprit", "error", "", time.Now())
	if err != nil {
		t.Fatalf("seed victim issue: %v", err)
	}

	perfRes, err := s.h.PerfIssues.Record(ctx, victimProject.ID, trace.Finding{
		Kind:        trace.KindNPlusOne,
		Culprit:     "GET /b3-victim",
		Fingerprint: "b3-victim-perf-fp",
		Description: "SELECT * FROM b3_victims WHERE id = ?",
		Evidence:    map[string]any{"count": 3, "total_ms": int64(30), "span_ids": []string{"s1"}},
	}, "b3-victim-trace")
	if err != nil {
		t.Fatalf("seed victim perf-issue: %v", err)
	}

	v := victimIDs{
		orgID:        victimOrg.ID,
		projectID:    victimProject.ID,
		monitorID:    victimMonitor.ID,
		statuspageID: victimSP.ID,
		teamID:       victimTeam.ID,
		issueID:      issueRes.IssueID,
		perfIssueID:  perfRes.Issue.ID,
	}

	exemptLeave := guards.ExemptedValues(leaveConfirmExemption)
	seenLeave := make(map[string]bool)

	tested := 0
	for _, route := range s.h.RegisteredRoutes() {
		method, path, ok := strings.Cut(route, " ")
		if !ok || !isStrangerScopedRoute(path) {
			continue
		}
		tested++
		concrete := concreteVictimPath(t, path, v)

		var resp *http.Response
		switch method {
		case http.MethodGet:
			resp = getWithCookie(t, s.srv, concrete, strangerCookie)
		case http.MethodPost:
			// Пустое тело + валидный Origin: без Origin любой POST лёг бы на
			// sameOrigin раньше проверяемого гейта (см. routes_origin_test.go)
			// и тест доказывал бы не то.
			resp = postForm(t, s.srv, concrete, url.Values{}, s.srv.URL, strangerCookie)
		default:
			t.Fatalf("маршрут %q: неожиданный метод %q — обнови тест", route, method)
			continue
		}
		code := statusOf(t, resp)
		if code != http.StatusNotFound && code != http.StatusForbidden {
			if exemptLeave[route] {
				seenLeave[route] = true
				continue
			}
			t.Errorf("%s %s (чужак, valid-but-foreign id) статус = %d, ожидали 404 или 403 — ГЕЙТ ПРОПУСТИЛ ЧУЖАКА", method, concrete, code)
		}
	}
	if tested == 0 {
		t.Fatal("не найдено ни одного project/org-scoped маршрута — предикат isStrangerScopedRoute сломан?")
	}
	guards.CheckExemptions(t, "TestAuthzBehaviorStrangerRejectedOnScopedRoutes", leaveConfirmExemption, 1, seenLeave)

	// leaveConfirmExemption покрывает только экран подтверждения (первый
	// POST без confirmed=yes) — настоящая мутация проверяется отдельно: с
	// confirmed=yes чужак обязан упереться в ErrNotMember → 404, как и
	// заявлено в authz_map_test.go.
	leavePath := "/orgs/" + strconv.FormatInt(v.orgID, 10) + "/settings/leave"
	leaveResp := postForm(t, s.srv, leavePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, strangerCookie)
	if code := statusOf(t, leaveResp); code != http.StatusNotFound {
		t.Errorf("POST %s confirmed=yes (чужак) статус = %d, ожидали 404 — РЕАЛЬНАЯ МУТАЦИЯ НЕ ЗАЩИЩЕНА", leavePath, code)
	}

	// Ни один POST выше не должен был мутировать ресурсы жертвы — перечитываем
	// каждый вид ресурса и сверяем с исходным состоянием.
	if got, err := orgSvc.GetProject(ctx, victimProject.ID); err != nil || got.Name != victimProject.Name {
		t.Errorf("victim project mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if got, err := orgSvc.Get(ctx, victimOrg.ID); err != nil || got.Name != victimOrg.Name {
		t.Errorf("victim org mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if got, err := s.uptime.Get(ctx, victimMonitor.ID); err != nil || !got.Enabled {
		t.Errorf("victim monitor mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if _, err := orgSvc.TeamOrg(ctx, victimTeam.ID); err != nil {
		t.Errorf("victim team deleted by stranger: err=%v", err)
	}
	// PublicID: единственный публичный адрес страницы, поля Slug у
	// StatusPage больше нет (T5, миграция 0063 удалила колонку). PublicID
	// неизменяем и реально хранится в БД — подходящий инвариант
	// identity-проверки.
	if got, err := s.uptime.StatusPageByID(ctx, victimSP.ID); err != nil || got.PublicID != victimSP.PublicID || !got.Enabled {
		t.Errorf("victim statuspage mutated/deleted by stranger: got=%+v err=%v", got, err)
	}
	if got, err := s.h.Issues.Get(ctx, issueRes.IssueID); err != nil || got.Status != "unresolved" {
		t.Errorf("victim issue mutated by stranger: got=%+v err=%v", got, err)
	}
	if got, err := s.h.PerfIssues.Get(ctx, victimProject.ID, perfRes.Issue.ID); err != nil || got.Status != "unresolved" {
		t.Errorf("victim perf-issue mutated by stranger: got=%+v err=%v", got, err)
	}
}

// TestAuthzBehaviorMemberRejectedOnAdminRoutes — K14-2: тест выше ловит
// ЧУЖАКА (нулевое членство в организации жертвы), но не проверяет вторую
// половину той же матрицы — СВОЕГО участника организации, у которого просто
// не хватает роли. Разрыв: карта прав (routeAuthz, authz_map_test.go)
// декларирует admin/owner/instance_admin уровень для десятков маршрутов, но
// ни один сторож не бьётся о requireOrgRole/requireProjectRole/
// requireOrgOwner/requireInstanceAdminForSSO РЕАЛЬНЫМ участником с ролью
// member — только полным чужаком, для которого 404 гарантирован уже на
// уровне «нет членства», ещё до проверки роли.
//
// Перебор — ПО КАРТЕ routeAuthz (тот же источник истины, что у
// TestRoutesDeclareAuthzLevel), а не списком маршрутов, набранным руками:
// новый admin-маршрут, объявленный в карте без гейта роли в хендлере, ловится
// автоматически, без правки этого теста.
//
// Два актёра — участник и «оператор» (участник + team-attachment к проекту
// жертвы, тот самый canOperateProject/CanAccessProject): у обоих role=member
// в организации, но identical-role недостаточно нарочно — задача теста
// доказать, что operate-доступ (через команду) не даёт administer-доступа.
func TestAuthzBehaviorMemberRejectedOnAdminRoutes(t *testing.T) {
	s := newUptimeStack(t)
	// alerts/channels/* (lvlAdmin) сначала проверяют h.Alerts на nil и
	// только потом гейт роли (тот же порядок, что объяснён в шапке файла) —
	// без него участник получал бы 404 от nil-guard'а, не долетев до
	// requireProjectRole, который и обязан проверить этот тест.
	s.h.Alerts = alert.NewService(s.pool)
	s.h.Outbox = notify.NewOutbox(s.pool)

	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	// Расходуем bootstrap instance-admin слот на одноразового пользователя
	// (см. комментарий у orgSettingsRegister выше) — иначе owner/member ниже
	// сами стали бы инстанс-админом и легитимно проходили бы SSO-ручки.
	orgSettingsRegister(t, authSvc, "k14-bootstrap-sink@example.com")

	ownerID, _ := orgSettingsRegister(t, authSvc, "k14-owner@example.com")
	victimOrg, err := orgSvc.CreateOrg(ctx, "k14-org", "K14 Org", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	victimProject, err := orgSvc.CreateProject(ctx, victimOrg.ID, "k14-proj", "K14 Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	victimTeam, err := orgSvc.CreateTeam(ctx, victimOrg.ID, "k14-team", "K14 Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	// Все admin/owner/instance_admin маршруты карты адресуют только проект,
	// организацию или команду (см. проверку ниже) — монитор/статус-
	// страница/issue/perf-issue victimIDs остаются нулевыми осознанно, они
	// этой матрицей не затрагиваются.
	v := victimIDs{orgID: victimOrg.ID, projectID: victimProject.ID, teamID: victimTeam.ID}

	type actor struct {
		label  string
		cookie *http.Cookie
	}
	var actors []actor

	memberID, memberCookie := orgSettingsRegister(t, authSvc, "k14-member@example.com")
	if err := orgSvc.AddMember(ctx, victimOrg.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	actors = append(actors, actor{"участник (role=member)", memberCookie})

	operatorID, operatorCookie := orgSettingsRegister(t, authSvc, "k14-operator@example.com")
	if err := orgSvc.AddMember(ctx, victimOrg.ID, operatorID, org.RoleMember); err != nil {
		t.Fatalf("add operator as member: %v", err)
	}
	if err := orgSvc.AddTeamMember(ctx, victimTeam.ID, operatorID); err != nil {
		t.Fatalf("add operator to team: %v", err)
	}
	if err := orgSvc.AttachTeam(ctx, victimProject.ID, victimTeam.ID); err != nil {
		t.Fatalf("attach team to project: %v", err)
	}
	actors = append(actors, actor{"оператор (team-attached, role=member, canOperateProject=true)", operatorCookie})

	tested := 0
	for _, a := range actors {
		for _, route := range s.h.RegisteredRoutes() {
			method, path, ok := strings.Cut(route, " ")
			if !ok || !isStrangerScopedRoute(path) {
				continue
			}
			lvl, known := routeAuthz[route]
			if !known || (lvl != lvlAdmin && lvl != lvlOwner && lvl != lvlInstanceAdmin) {
				continue
			}
			tested++
			concrete := concreteVictimPath(t, path, v)

			var resp *http.Response
			switch method {
			case http.MethodGet:
				resp = getWithCookie(t, s.srv, concrete, a.cookie)
			case http.MethodPost:
				resp = postForm(t, s.srv, concrete, url.Values{}, s.srv.URL, a.cookie)
			default:
				t.Fatalf("маршрут %q: неожиданный метод %q — обнови тест", route, method)
				continue
			}
			code := statusOf(t, resp)
			if code != http.StatusNotFound && code != http.StatusForbidden {
				t.Errorf("%s %s (%s, своя организация, недостаточная роль) статус = %d, ожидали 404 или 403 — ГЕЙТ ПРОПУСТИЛ УЧАСТНИКА НА ADMIN-МАРШРУТ", method, concrete, a.label, code)
			}
		}
	}
	if tested == 0 {
		t.Fatal("не найдено ни одного admin/owner/instance_admin маршрута с адресацией по id в routeAuthz — фильтр или карта сломаны?")
	}
}

// TestAuthzBehaviorRoleNotSharedAcrossOrgs — K14-3: один и тот же пользователь
// (dual) — admin в организации A и рядовой member в организации B. Классический
// источник межарендной утечки: где-то роль резолвится/кешируется без привязки
// к КОНКРЕТНОЙ организации ("этот юзер вообще admin?" вместо "admin ЭТОЙ
// организации?"), и права из A протекают в B. Проверка — не на одном
// маршруте, а на тройке разных по природе действий: чтение (доступ к чужому
// для роли member проекту через легитимный team-attachment), изменение
// (мутация внутри уже доступного проекта — уровень участника, не админа) и
// административное действие (rename проекта). Последнее проверяется дважды —
// в СВОЕЙ организации A (позитивный контроль: admin действительно может, иначе
// отказ в B ничего не доказывает) и в ЧУЖОЙ B (должен быть отказ).
func TestAuthzBehaviorRoleNotSharedAcrossOrgs(t *testing.T) {
	s := newUptimeStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	// Расходуем bootstrap instance-admin слот, как в остальных тестах файла.
	orgSettingsRegister(t, authSvc, "k14x-bootstrap-sink@example.com")

	dualID, dualCookie := orgSettingsRegister(t, authSvc, "k14x-dual@example.com")

	// Организация A: dual — admin.
	ownerA, _ := orgSettingsRegister(t, authSvc, "k14x-owner-a@example.com")
	orgA, err := orgSvc.CreateOrg(ctx, "k14x-org-a", "K14x Org A", ownerA)
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	if err := orgSvc.AddMember(ctx, orgA.ID, dualID, org.RoleAdmin); err != nil {
		t.Fatalf("add dual as admin of A: %v", err)
	}
	projA, err := orgSvc.CreateProject(ctx, orgA.ID, "k14x-proj-a", "K14x Proj A", "go")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}

	// Организация B (жертва): dual — обычный member, доступ к проекту только
	// через команду, как у любого рядового участника (та же граница, что и у
	// оператора в тесте выше).
	ownerB, _ := orgSettingsRegister(t, authSvc, "k14x-owner-b@example.com")
	orgB, err := orgSvc.CreateOrg(ctx, "k14x-org-b", "K14x Org B", ownerB)
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	if err := orgSvc.AddMember(ctx, orgB.ID, dualID, org.RoleMember); err != nil {
		t.Fatalf("add dual as member of B: %v", err)
	}
	projB, err := orgSvc.CreateProject(ctx, orgB.ID, "k14x-proj-b", "K14x Proj B", "go")
	if err != nil {
		t.Fatalf("create project B: %v", err)
	}
	teamB, err := orgSvc.CreateTeam(ctx, orgB.ID, "k14x-team-b", "K14x Team B")
	if err != nil {
		t.Fatalf("create team B: %v", err)
	}
	if err := orgSvc.AddTeamMember(ctx, teamB.ID, dualID); err != nil {
		t.Fatalf("attach dual to team B: %v", err)
	}
	if err := orgSvc.AttachTeam(ctx, projB.ID, teamB.ID); err != nil {
		t.Fatalf("attach team B to project B: %v", err)
	}

	// Инвариант на уровне сервиса, отдельно от web: роль резолвится ПО
	// ОРГАНИЗАЦИИ, а не по пользователю — сентинел на случай, если Role()
	// когда-нибудь обзаведётся кешем/срезом без ключа по org_id.
	if role, rerr := orgSvc.Role(ctx, orgA.ID, dualID); rerr != nil || role != org.RoleAdmin {
		t.Fatalf("Role(orgA, dual) = %v, %v, want RoleAdmin, nil", role, rerr)
	}
	if role, rerr := orgSvc.Role(ctx, orgB.ID, dualID); rerr != nil || role != org.RoleMember {
		t.Fatalf("Role(orgB, dual) = %v, %v, want RoleMember, nil", role, rerr)
	}

	// --- Чтение: dual видит проект B, потому что реально в нём состоит
	// (team-attachment) — легитимный member-доступ, не утечка. Список пуст
	// (issue заводим ниже) — сознательно: страница считает CH-спарклайны
	// только когда issues есть (h.Events здесь не поднят, как и в остальных
	// тестах файла без ClickHouse-зависимых страниц), а сама проверка
	// касается только доступа к списку, не его содержимого. ---
	issuesBPath := "/projects/" + strconv.FormatInt(projB.ID, 10) + "/issues"
	if code := statusOf(t, getWithCookie(t, s.srv, issuesBPath, dualCookie)); code != http.StatusOK {
		t.Errorf("GET %s (dual, легитимный team-доступ в B) = %d, want 200", issuesBPath, code)
	}

	// --- Изменение: мутация внутри уже доступного проекта B — уровень
	// участника (CanAccessProject), не админа. ---
	issueB, err := s.h.Issues.Upsert(ctx, projB.ID, "k14x-fp", "K14x issue", "k14x.Culprit", "error", "", time.Now())
	if err != nil {
		t.Fatalf("seed issue B: %v", err)
	}
	issueStatusPath := "/issues/" + strconv.FormatInt(issueB.IssueID, 10) + "/status"
	statusResp := postForm(t, s.srv, issueStatusPath, url.Values{"status": {"resolved"}}, s.srv.URL, dualCookie)
	if code := statusOf(t, statusResp); code != http.StatusSeeOther {
		t.Errorf("POST %s (dual, легитимный team-доступ в B) = %d, want 303", issueStatusPath, code)
	}

	// --- Административное действие в СВОЕЙ организации (позитивный
	// контроль): admin действительно может переименовать проект A — если бы
	// это не проходило, отказ по проекту B ниже ничего бы не доказывал. ---
	renamePathA := "/projects/" + strconv.FormatInt(projA.ID, 10) + "/settings/rename"
	renameA := postForm(t, s.srv, renamePathA, url.Values{"name": {"K14x Proj A Renamed"}}, s.srv.URL, dualCookie)
	if code := statusOf(t, renameA); code != http.StatusSeeOther {
		t.Fatalf("POST %s (dual, реальный admin организации A) = %d, want 303 — контроль сломан, дальнейший результат недостоверен", renamePathA, code)
	}

	// --- Административное действие в ЧУЖОЙ организации: dual — admin
	// организации A, но не B, и не должен мочь переименовать проект B. ---
	renamePathB := "/projects/" + strconv.FormatInt(projB.ID, 10) + "/settings/rename"
	renameB := postForm(t, s.srv, renamePathB, url.Values{"name": {"k14x-hijacked"}}, s.srv.URL, dualCookie)
	if code := statusOf(t, renameB); code != http.StatusNotFound && code != http.StatusForbidden {
		t.Errorf("POST %s (dual, admin ЧУЖОЙ организации A) статус = %d, ожидали 404 или 403 — РОЛЬ ИЗ ОРГАНИЗАЦИИ A ПРОТЕКЛА В B", renamePathB, code)
	}
	if got, gerr := orgSvc.GetProject(ctx, projB.ID); gerr != nil || got.Name != projB.Name {
		t.Errorf("project B переименован admin'ом чужой организации: got=%+v err=%v", got, gerr)
	}
}
