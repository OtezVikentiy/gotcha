package org_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestTeamsOf(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "teamsof-owner@example.com")
	o, err := svc.CreateOrg(ctx, "teamsof-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	zTeam, err := svc.CreateTeam(ctx, o.ID, "zeta", "Zeta")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	aTeam, err := svc.CreateTeam(ctx, o.ID, "alpha", "Alpha")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	// Другая организация не должна протекать в выборку.
	otherOwner := newUser(t, pool, "teamsof-other@example.com")
	otherOrg, err := svc.CreateOrg(ctx, "teamsof-other-org", "Other", otherOwner)
	if err != nil {
		t.Fatalf("CreateOrg (other): %v", err)
	}
	if _, err := svc.CreateTeam(ctx, otherOrg.ID, "outside", "Outside"); err != nil {
		t.Fatalf("CreateTeam (other): %v", err)
	}

	teams, err := svc.TeamsOf(ctx, o.ID)
	if err != nil {
		t.Fatalf("TeamsOf: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("TeamsOf = %+v, want 2 teams", teams)
	}
	if teams[0].ID != aTeam.ID || teams[1].ID != zTeam.ID {
		t.Errorf("TeamsOf order = %+v, want alpha before zeta", teams)
	}
}

func TestTeamMembers(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "teammembers-owner@example.com")
	o, err := svc.CreateOrg(ctx, "teammembers-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	team, err := svc.CreateTeam(ctx, o.ID, "core", "Core")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	zDev := newUser(t, pool, "zzz-dev@example.com")
	aDev := newUser(t, pool, "aaa-dev@example.com")
	for _, uid := range []int64{zDev, aDev} {
		if err := svc.AddMember(ctx, o.ID, uid, org.RoleMember); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
		if err := svc.AddTeamMember(ctx, team.ID, uid); err != nil {
			t.Fatalf("AddTeamMember: %v", err)
		}
	}
	// Участник организации, но не команды, не должен попасть в выборку.
	outsider := newUser(t, pool, "not-in-team@example.com")
	if err := svc.AddMember(ctx, o.ID, outsider, org.RoleMember); err != nil {
		t.Fatalf("AddMember (outsider): %v", err)
	}

	members, err := svc.TeamMembers(ctx, team.ID)
	if err != nil {
		t.Fatalf("TeamMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("TeamMembers = %+v, want 2 members", members)
	}
	if members[0].Email != "aaa-dev@example.com" || members[1].Email != "zzz-dev@example.com" {
		t.Errorf("TeamMembers order = %+v, want sorted by email", members)
	}
	if members[0].Role != org.RoleMember {
		t.Errorf("TeamMembers[0].Role = %q, want member", members[0].Role)
	}
}

func TestTeamProjects(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "teamprojects-owner@example.com")
	o, err := svc.CreateOrg(ctx, "teamprojects-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	team, err := svc.CreateTeam(ctx, o.ID, "backend", "Backend")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	api, err := svc.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := svc.CreateProject(ctx, o.ID, "unattached", "Unattached", "go"); err != nil {
		t.Fatalf("CreateProject (unattached): %v", err)
	}
	if err := svc.AttachTeam(ctx, api.ID, team.ID); err != nil {
		t.Fatalf("AttachTeam: %v", err)
	}

	projects, err := svc.TeamProjects(ctx, team.ID)
	if err != nil {
		t.Fatalf("TeamProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != api.ID {
		t.Fatalf("TeamProjects = %+v, want only api", projects)
	}
}

func TestTeamOrg(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "teamorg-owner@example.com")
	o, err := svc.CreateOrg(ctx, "teamorg-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	team, err := svc.CreateTeam(ctx, o.ID, "backend", "Backend")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	gotOrgID, err := svc.TeamOrg(ctx, team.ID)
	if err != nil || gotOrgID != o.ID {
		t.Fatalf("TeamOrg(%d) = %d, err=%v, want %d, nil", team.ID, gotOrgID, err, o.ID)
	}

	if _, err := svc.TeamOrg(ctx, team.ID+1_000_000); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("TeamOrg (missing): got %v, want ErrNotFound", err)
	}
}

func TestRemoveTeamMember(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "removeteammember-owner@example.com")
	o, err := svc.CreateOrg(ctx, "removeteammember-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	team, err := svc.CreateTeam(ctx, o.ID, "core", "Core")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	dev := newUser(t, pool, "removeteammember-dev@example.com")
	if err := svc.AddMember(ctx, o.ID, dev, org.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := svc.AddTeamMember(ctx, team.ID, dev); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}

	if err := svc.RemoveTeamMember(ctx, team.ID, dev); err != nil {
		t.Fatalf("RemoveTeamMember: %v", err)
	}
	members, err := svc.TeamMembers(ctx, team.ID)
	if err != nil || len(members) != 0 {
		t.Fatalf("TeamMembers after remove = %+v err=%v, want empty", members, err)
	}

	// Повторное удаление (или удаление того, кто и не состоял) → ErrNotMember.
	if err := svc.RemoveTeamMember(ctx, team.ID, dev); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("RemoveTeamMember (already gone): got %v, want ErrNotMember", err)
	}
}
