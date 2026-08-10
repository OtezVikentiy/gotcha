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
)

// TestMemberSeesNoLinksToPagesThatRejectHim — сквозная проверка того, что
// участнику не показывают ссылок на страницы, которые ему отдают 404.
//
// Правила по метрикам, окна обслуживания и статус-страницы требуют
// owner/admin. Навигация показывала их всем ролям: человек тыкал в пункт,
// нарисованный самим продуктом, и попадал на «страницы нет». Туда же вела
// ссылка «Настройки приёма» из баннера квоты.
//
// Алерты сюда больше не входят: с задачи 5 (спека 2026-08-08) страница
// алертов и сохранение правил открыты оператору (requireProjectOperator) —
// участник команды, что и используется этим тестом, теперь И ЕСТЬ оператор,
// и 200 для него ожидаемо (сама ссылка в навигации — задача 7, nav.CanOperate,
// эта проверка её не утверждает ни в одну сторону).
func TestMemberSeesNoLinksToPagesThatRejectHim(t *testing.T) {
	s := newStack(t)
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
	resp := getWithCookie(t, s.srv, "/projects/"+pid+"/issues", memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET issues как участник = %d, want 200", resp.StatusCode)
	}
	page := string(body)

	// Ни одной ссылки на страницу, закрытую для участника.
	forbidden := []string{
		"/projects/" + pid + "/maintenance",
		"/projects/" + pid + "/statuspages",
		"/projects/" + pid + "/metrics/alerts",
		"/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings",
	}
	for _, href := range forbidden {
		if strings.Contains(page, `href="`+href+`"`) {
			t.Errorf("участнику показана ссылка %q, которая отдаёт ему отказ", href)
		}
	}

	// И контрольная проверка, что эти страницы действительно закрыты — иначе
	// тест защищал бы от несуществующей проблемы. Члену организации с
	// недостаточной ролью отвечает честный 403 (№72), не 404: членство ему
	// и так известно. В ЭТОМ стенде не подняты Uptime/MetricRules, поэтому
	// maintenance и metrics/alerts упираются в nil-guard (404) раньше
	// проверки роли — оба кода означают «закрыто».
	for _, path := range forbidden {
		r := getWithCookie(t, s.srv, path, memberCookie)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden && r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s как участник = %d, want 403/404 (иначе прятать ссылку незачем)", path, r.StatusCode)
		}
	}

	// Доступное участнику не пропало: область «Аптайм» в рейле ведёт на первый
	// её подраздел — мониторы, — и он участнику открыт.
	if !strings.Contains(page, `href="/projects/`+pid+`/monitors"`) {
		t.Error("участник потерял область «Аптайм» — мониторы ему доступны")
	}
}
