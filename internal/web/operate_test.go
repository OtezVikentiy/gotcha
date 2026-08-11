package web

import (
	"context"
	"net/http/httptest"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRequireProjectOperatorReturnsAuthz — B4: гейт теперь возвращает
// projectAuthz{OrgID, CanManage} вместо голого orgID, чтобы рендеры не
// резолвили CanManage заново отдельным canManageProject. Проверяем оба
// значения CanManage на одном и том же проекте: участник команды (оператор,
// доступ через team, не owner/admin) получает CanManage=false, admin
// организации — CanManage=true; OrgID совпадает с созданной организацией в
// обоих случаях.
func TestRequireProjectOperatorReturnsAuthz(t *testing.T) {
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	h := &Handler{Org: orgSvc}
	ctx := context.Background()

	ownerID, err := authSvc.Register(ctx, "authz-owner@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	adminID, err := authSvc.Register(ctx, "authz-admin@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	operatorID, err := authSvc.Register(ctx, "authz-operator@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}

	o, err := orgSvc.CreateOrg(ctx, "authz-co", "Authz Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, o.ID, adminID, org.RoleAdmin); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	// operatorID — доступ к проекту только через команду, не owner/admin
	// (тот же приём, что addTeamAccess в monitors_test.go): AddTeamMember
	// требует предварительного org_members-членства (FK), поэтому сперва
	// обычный RoleMember.
	if err := orgSvc.AddMember(ctx, o.ID, operatorID, org.RoleMember); err != nil {
		t.Fatalf("add operator as member: %v", err)
	}
	proj, err := orgSvc.CreateProject(ctx, o.ID, "authz-proj", "Authz Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	team, err := orgSvc.CreateTeam(ctx, o.ID, "authz-team", "authz-team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := orgSvc.AddTeamMember(ctx, team.ID, operatorID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := orgSvc.AttachTeam(ctx, proj.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	r := httptest.NewRequest("GET", "/", nil)

	authz, ok := h.requireProjectOperator(httptest.NewRecorder(), r, proj.ID, operatorID)
	if !ok {
		t.Fatalf("operator: requireProjectOperator ok = false, want true")
	}
	if authz.OrgID != o.ID {
		t.Errorf("operator: OrgID = %d, want %d", authz.OrgID, o.ID)
	}
	if authz.CanManage {
		t.Errorf("operator: CanManage = true, want false (team member, not owner/admin)")
	}

	authz, ok = h.requireProjectOperator(httptest.NewRecorder(), r, proj.ID, adminID)
	if !ok {
		t.Fatalf("admin: requireProjectOperator ok = false, want true")
	}
	if authz.OrgID != o.ID {
		t.Errorf("admin: OrgID = %d, want %d", authz.OrgID, o.ID)
	}
	if !authz.CanManage {
		t.Errorf("admin: CanManage = false, want true")
	}
}
