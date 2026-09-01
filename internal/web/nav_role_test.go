package web_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestMemberSeesNoLinksToPagesThatRejectHim — сквозная проверка того, что
// участнику не показывают ссылок на страницы, которые ему отдают отказ.
//
// Ревью фикс-раунда 2: в этой кодовой базе canOperateProject буквально
// равен CanAccessProject (internal/web/operate.go) — состояния «видит
// проект, но не оператор» не существует физически: чтобы вообще увидеть
// проект, участник обязан быть в команде, привязанной к проекту
// (accessCondition, internal/org/project.go), а это и есть определение
// оператора. Поэтому единственная реальная граница для НЕ-owner/admin
// участника — CanManage (lvlAdmin в authz_map_test.go): «Настройки
// проекта», «Участники/Команды/Пробы» организации. Все lvlOperator-пункты
// (maintenance, статус-страницы, alerts-группа целиком) участнику КОМАНДЫ
// теперь показываются легитимно — тот же уровень, что и у GET-ручек этих
// страниц (сверено построчно с authz_map_test.go: rules_errors/metric_alerts/
// host_thresholds/slo/maintenance/alert_suppression/escalations/
// alert_deliveries/status_pages — все lvlOperator, project_settings и
// org settings/teams/probes — все lvlAdmin, gate в nav.go — тот же
// CanOperate/CanManage).
func TestMemberSeesNoLinksToPagesThatRejectHim(t *testing.T) {
	s := newStack(t)
	// /statuspages ниже нужен рендер полного ctx-сайдбара (не chromeless,
	// как у /setup) без 404 на nil-guard — h.Uptime здесь не поднят
	// newStack по умолчанию (стенд для алертов/квот), заводим только для
	// этого теста.
	s.h.Uptime = uptime.NewService(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, _ := orgSettingsRegister(t, authSvc, "navrole-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "navrole-member@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "navrole-co", "Nav Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "navrole-proj", "Nav Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, orgSvc, o.ID, proj.ID, memberID, "navrole-team")

	pid := strconv.FormatInt(proj.ID, 10)
	oid := strconv.FormatInt(o.ID, 10)
	resp := getWithCookie(t, s.srv, "/projects/"+pid+"/issues", memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET issues как участник = %d, want 200", resp.StatusCode)
	}
	page := string(body)

	// Ни одной ссылки на страницу, закрытую для участника БЕЗ CanManage
	// (owner/admin организации).
	forbidden := []string{
		"/projects/" + pid + "/settings",
		"/orgs/" + oid + "/settings",
		"/orgs/" + oid + "/teams",
		"/orgs/" + oid + "/probes",
	}
	for _, href := range forbidden {
		if strings.Contains(page, `href="`+href+`"`) {
			t.Errorf("участнику показана ссылка %q, которая отдаёт ему отказ", href)
		}
	}

	// И контрольная проверка, что эти страницы действительно закрыты — иначе
	// тест защищал бы от несуществующей проблемы. Члену организации с
	// недостаточной ролью отвечает честный 403 (№72), не 404: членство ему
	// и так известно.
	for _, path := range forbidden {
		r := getWithCookie(t, s.srv, path, memberCookie)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden && r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s как участник = %d, want 403/404 (иначе прятать ссылку незачем)", path, r.StatusCode)
		}
	}

	// Симметрично: участник КОМАНДЫ — оператор проекта (см. докблок), и
	// иконки рейла для lvlOperator-областей легитимно ведут его внутрь, а не
	// прячутся. «Оповещения» ведёт на первый подраздел (rules_errors,
	// CanOperate); «Настройки» — на его первый ВИДИМЫЙ подраздел: без
	// CanManage «Настройки проекта» отфильтрованы, и первым остаётся
	// «Статус-страницы» (CanOperate) — то самое /statuspages, которое до
	// фикса тест ошибочно считал закрытым для участника команды.
	if !strings.Contains(page, `href="/projects/`+pid+`/alerts"`) {
		t.Error("участнику команды не показана область «Оповещения» — регрессия задачи 5")
	}
	if !strings.Contains(page, `href="/projects/`+pid+`/statuspages"`) {
		t.Error("участнику команды не показан первый видимый подраздел «Настроек» (статус-страницы)")
	}

	// Доступное участнику не пропало: область «Аптайм» в рейле ведёт на первый
	// её подраздел — мониторы, — и он участнику открыт.
	if !strings.Contains(page, `href="/projects/`+pid+`/monitors"`) {
		t.Error("участник потерял область «Аптайм» — мониторы ему доступны")
	}

	// Контекстная колонка группы «Настройки» рендерится только НА странице
	// области "settings" (ctxNav — подразделы ТЕКУЩЕЙ области, не рейл) — а
	// /issues туда не заходит. /statuspages — lvlOperator (h.Uptime заведён
	// выше специально для этого теста), её сайдбар обязан показать
	// «Статус-страницы»/«Первые шаги» (CanOperate/без гейта), но НЕ
	// «Настройки проекта» и группу «Организация» (Members/Teams/Probes) —
	// оба CanManage-only. (/setup рендерится отдельным chromeless-layout
	// без ctx-сайдбара вовсе — не годится для этой проверки.)
	settingsResp := getWithCookie(t, s.srv, "/projects/"+pid+"/statuspages", memberCookie)
	settingsBody, _ := io.ReadAll(settingsResp.Body)
	settingsResp.Body.Close()
	if settingsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET statuspages как оператор команды = %d, want 200", settingsResp.StatusCode)
	}
	settingsPage := string(settingsBody)
	for _, href := range []string{
		"/orgs/" + oid + "/settings",
		"/orgs/" + oid + "/teams",
		"/orgs/" + oid + "/probes",
		"/projects/" + pid + "/settings",
	} {
		if strings.Contains(settingsPage, `href="`+href+`"`) {
			t.Errorf("сайдбар «Настроек» показывает участнику команды CanManage-only ссылку %q", href)
		}
	}
	if !strings.Contains(settingsPage, `href="/projects/`+pid+`/statuspages"`) {
		t.Error("сайдбар «Настроек» не содержит «Статус-страницы» (CanOperate) для оператора")
	}
	if !strings.Contains(settingsPage, `href="/projects/`+pid+`/setup"`) {
		t.Error("сайдбар «Настроек» не содержит саму себя («Первые шаги», доступна без гейта)")
	}
}
