package org_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRemoveMemberRevokesTeamProjectAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	ownerID := newUser(t, pool, "tenant-owner@example.com")
	memberID := newUser(t, pool, "tenant-member@example.com")
	o, err := svc.CreateOrg(ctx, "tenant", "Tenant", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := svc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	p, err := svc.CreateProject(ctx, o.ID, "app", "App", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	tm, err := svc.CreateTeam(ctx, o.ID, "core", "Core")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := svc.AddTeamMember(ctx, tm.ID, memberID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := svc.AttachTeam(ctx, p.ID, tm.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	ok, err := svc.CanAccessProject(ctx, memberID, p.ID)
	if err != nil || !ok {
		t.Fatalf("до удаления доступ должен быть: ok=%v err=%v", ok, err)
	}

	if err := svc.RemoveMember(ctx, o.ID, memberID); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	ok, err = svc.CanAccessProject(ctx, memberID, p.ID)
	if err != nil {
		t.Fatalf("can access after removal: %v", err)
	}
	if ok {
		t.Fatal("исключённый участник сохранил доступ к проекту своей команды")
	}
	projs, err := svc.ProjectsForUser(ctx, memberID)
	if err != nil {
		t.Fatalf("projects for user: %v", err)
	}
	if len(projs) != 0 {
		t.Fatalf("ProjectsForUser = %d проектов, want 0", len(projs))
	}
}

func TestRemoveMemberAsRevokesTeamProjectAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	ownerID := newUser(t, pool, "tenant-as-owner@example.com")
	memberID := newUser(t, pool, "tenant-as-member@example.com")
	o, err := svc.CreateOrg(ctx, "tenant-as", "Tenant As", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := svc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	p, err := svc.CreateProject(ctx, o.ID, "app", "App", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	tm, err := svc.CreateTeam(ctx, o.ID, "core", "Core")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := svc.AddTeamMember(ctx, tm.ID, memberID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := svc.AttachTeam(ctx, p.ID, tm.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	ok, err := svc.CanAccessProject(ctx, memberID, p.ID)
	if err != nil || !ok {
		t.Fatalf("до удаления доступ должен быть: ok=%v err=%v", ok, err)
	}

	if err := svc.RemoveMemberAs(ctx, o.ID, ownerID, memberID); err != nil {
		t.Fatalf("remove member as: %v", err)
	}

	ok, err = svc.CanAccessProject(ctx, memberID, p.ID)
	if err != nil {
		t.Fatalf("can access after removal: %v", err)
	}
	if ok {
		t.Fatal("исключённый актёром-владельцем участник сохранил доступ к проекту своей команды")
	}
	projs, err := svc.ProjectsForUser(ctx, memberID)
	if err != nil {
		t.Fatalf("projects for user: %v", err)
	}
	if len(projs) != 0 {
		t.Fatalf("ProjectsForUser = %d проектов, want 0", len(projs))
	}
}

func TestLeaveOrgRevokesTeamProjectAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	ownerID := newUser(t, pool, "tenant-leave-owner@example.com")
	memberID := newUser(t, pool, "tenant-leave-member@example.com")
	o, err := svc.CreateOrg(ctx, "tenant-leave", "Tenant Leave", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := svc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	p, err := svc.CreateProject(ctx, o.ID, "app", "App", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	tm, err := svc.CreateTeam(ctx, o.ID, "core", "Core")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := svc.AddTeamMember(ctx, tm.ID, memberID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := svc.AttachTeam(ctx, p.ID, tm.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	ok, err := svc.CanAccessProject(ctx, memberID, p.ID)
	if err != nil || !ok {
		t.Fatalf("до выхода доступ должен быть: ok=%v err=%v", ok, err)
	}

	if err := svc.RemoveMember(ctx, o.ID, memberID); err != nil {
		t.Fatalf("leave org: %v", err)
	}

	ok, err = svc.CanAccessProject(ctx, memberID, p.ID)
	if err != nil {
		t.Fatalf("can access after leave: %v", err)
	}
	if ok {
		t.Fatal("вышедший из организации участник сохранил доступ к проекту своей команды")
	}
	projs, err := svc.ProjectsForUser(ctx, memberID)
	if err != nil {
		t.Fatalf("projects for user: %v", err)
	}
	if len(projs) != 0 {
		t.Fatalf("ProjectsForUser = %d проектов, want 0", len(projs))
	}
}

func TestTeamMembershipRequiresOrgMembership(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	ownerID := newUser(t, pool, "inv-owner@example.com")
	outsiderID := newUser(t, pool, "inv-outsider@example.com")
	o, err := svc.CreateOrg(ctx, "inv", "Inv", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	tm, err := svc.CreateTeam(ctx, o.ID, "core", "Core")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	_, err = pool.Exec(ctx,
		"INSERT INTO team_members (team_id, org_id, user_id) VALUES ($1, $2, $3)",
		tm.ID, o.ID, outsiderID)
	if err == nil {
		t.Fatal("вставка членства для не-участника организации прошла: инвариант не в схеме")
	}
	if !strings.Contains(err.Error(), "team_members_member_fk") {
		t.Fatalf("упало не на том ограничении: %v", err)
	}
}

func TestTeamMemberCannotBelongToForeignOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	aOwner := newUser(t, pool, "foreign-a@example.com")
	bOwner := newUser(t, pool, "foreign-b@example.com")
	orgA, err := svc.CreateOrg(ctx, "fa", "FA", aOwner)
	if err != nil {
		t.Fatalf("create org a: %v", err)
	}
	orgB, err := svc.CreateOrg(ctx, "fb", "FB", bOwner)
	if err != nil {
		t.Fatalf("create org b: %v", err)
	}
	teamA, err := svc.CreateTeam(ctx, orgA.ID, "core", "Core")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	_, err = pool.Exec(ctx,
		"INSERT INTO team_members (team_id, org_id, user_id) VALUES ($1, $2, $3)",
		teamA.ID, orgB.ID, bOwner)
	if err == nil {
		t.Fatal("членство в команде чужой организации создалось")
	}
	if !strings.Contains(err.Error(), "team_members_team_org_fk") {
		t.Fatalf("упало не на том ограничении: %v", err)
	}
}
