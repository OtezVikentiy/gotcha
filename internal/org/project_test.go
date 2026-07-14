package org_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestProjectsAndAccess(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "boss@example.com")
	o, err := svc.CreateOrg(ctx, "acme", "Acme", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	backend, err := svc.CreateTeam(ctx, o.ID, "backend", "Backend")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := svc.CreateTeam(ctx, o.ID, "backend", "Dup"); !errors.Is(err, org.ErrSlugTaken) {
		t.Fatalf("duplicate team slug: got %v", err)
	}

	api, err := svc.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	site, err := svc.CreateProject(ctx, o.ID, "site", "Site", "javascript")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := svc.AttachTeam(ctx, api.ID, backend.ID); err != nil {
		t.Fatalf("AttachTeam: %v", err)
	}

	// dev — member организации, состоит в backend → видит только api.
	dev := newUser(t, pool, "dev2@example.com")
	if err := svc.AddMember(ctx, o.ID, dev, org.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := svc.AddTeamMember(ctx, backend.ID, dev); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}

	// Чужака в команду добавить нельзя.
	outsider := newUser(t, pool, "outsider@example.com")
	if err := svc.AddTeamMember(ctx, backend.ID, outsider); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("outsider in team: got %v, want ErrNotMember", err)
	}

	devProjects, err := svc.ProjectsForUser(ctx, dev)
	if err != nil || len(devProjects) != 1 || devProjects[0].ID != api.ID {
		t.Fatalf("dev projects = %+v err=%v, want only api", devProjects, err)
	}
	ownerProjects, err := svc.ProjectsForUser(ctx, owner)
	if err != nil || len(ownerProjects) != 2 {
		t.Fatalf("owner projects = %+v err=%v, want both", ownerProjects, err)
	}

	for _, tc := range []struct {
		user, project int64
		want          bool
	}{
		{dev, api.ID, true},
		{dev, site.ID, false},
		{owner, site.ID, true},
		{outsider, api.ID, false},
	} {
		got, err := svc.CanAccessProject(ctx, tc.user, tc.project)
		if err != nil || got != tc.want {
			t.Errorf("CanAccessProject(user=%d, project=%d) = %v err=%v, want %v",
				tc.user, tc.project, got, err, tc.want)
		}
	}
}

func TestCreateTeamInvalidSlug(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "teamslugowner@example.com")
	o, err := svc.CreateOrg(ctx, "teamslugorg", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := svc.CreateTeam(ctx, o.ID, "Bad Slug!", "Team"); !errors.Is(err, org.ErrInvalidSlug) {
		t.Fatalf("CreateTeam bad slug: got %v, want ErrInvalidSlug", err)
	}
}

func TestCreateProjectInvalidSlug(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "projslugowner@example.com")
	o, err := svc.CreateOrg(ctx, "projslugorg", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := svc.CreateProject(ctx, o.ID, "Bad Slug!", "Project", "go"); !errors.Is(err, org.ErrInvalidSlug) {
		t.Fatalf("CreateProject bad slug: got %v, want ErrInvalidSlug", err)
	}
}

func TestProjectOrg(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "projectorg-owner@example.com")
	o, err := svc.CreateOrg(ctx, "projectorg-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	p, err := svc.CreateProject(ctx, o.ID, "projectorg-proj", "Project", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	gotOrgID, err := svc.ProjectOrg(ctx, p.ID)
	if err != nil || gotOrgID != o.ID {
		t.Fatalf("ProjectOrg(%d) = %d, err=%v, want %d, nil", p.ID, gotOrgID, err, o.ID)
	}

	if _, err := svc.ProjectOrg(ctx, p.ID+1_000_000); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("ProjectOrg (missing): got %v, want ErrNotFound", err)
	}
}

func TestProjectsOf(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "projectsof-owner@example.com")
	o, err := svc.CreateOrg(ctx, "projectsof-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	zProj, err := svc.CreateProject(ctx, o.ID, "zeta", "Zeta", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	aProj, err := svc.CreateProject(ctx, o.ID, "alpha", "Alpha", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Другая организация не должна протекать в выборку.
	otherOwner := newUser(t, pool, "projectsof-other@example.com")
	otherOrg, err := svc.CreateOrg(ctx, "projectsof-other-org", "Other", otherOwner)
	if err != nil {
		t.Fatalf("CreateOrg (other): %v", err)
	}
	if _, err := svc.CreateProject(ctx, otherOrg.ID, "outside", "Outside", "go"); err != nil {
		t.Fatalf("CreateProject (other): %v", err)
	}

	projects, err := svc.ProjectsOf(ctx, o.ID)
	if err != nil {
		t.Fatalf("ProjectsOf: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("ProjectsOf = %+v, want 2 projects", projects)
	}
	if projects[0].ID != aProj.ID || projects[1].ID != zProj.ID {
		t.Errorf("ProjectsOf order = %+v, want alpha before zeta", projects)
	}
}

func TestDetachTeam(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "detachteam-owner@example.com")
	o, err := svc.CreateOrg(ctx, "detachteam-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	team, err := svc.CreateTeam(ctx, o.ID, "backend", "Backend")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	proj, err := svc.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := svc.AttachTeam(ctx, proj.ID, team.ID); err != nil {
		t.Fatalf("AttachTeam: %v", err)
	}

	if err := svc.DetachTeam(ctx, proj.ID, team.ID); err != nil {
		t.Fatalf("DetachTeam: %v", err)
	}
	projects, err := svc.TeamProjects(ctx, team.ID)
	if err != nil || len(projects) != 0 {
		t.Fatalf("TeamProjects after detach = %+v err=%v, want empty", projects, err)
	}

	// Идемпотентно: повторный DetachTeam (или detach несуществующей связи) → nil.
	if err := svc.DetachTeam(ctx, proj.ID, team.ID); err != nil {
		t.Fatalf("DetachTeam (already detached): got %v, want nil", err)
	}
}

func TestRenameProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "renameproject-owner@example.com")
	o, err := svc.CreateOrg(ctx, "renameproject-org", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	proj, err := svc.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := svc.RenameProject(ctx, proj.ID, "New Name"); err != nil {
		t.Fatalf("RenameProject: %v", err)
	}
	got, err := pool.Query(ctx, "SELECT name FROM projects WHERE id = $1", proj.ID)
	if err != nil {
		t.Fatalf("query name: %v", err)
	}
	defer got.Close()
	var name string
	if got.Next() {
		if err := got.Scan(&name); err != nil {
			t.Fatalf("scan name: %v", err)
		}
	}
	if name != "New Name" {
		t.Fatalf("name after rename = %q, want %q", name, "New Name")
	}

	if err := svc.RenameProject(ctx, proj.ID, ""); !errors.Is(err, org.ErrInvalidName) {
		t.Fatalf("RenameProject empty name: got %v, want ErrInvalidName", err)
	}
	if err := svc.RenameProject(ctx, proj.ID+1_000_000, "Whatever"); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("RenameProject missing project: got %v, want ErrNotFound", err)
	}
}

func TestAddTeamMemberIdempotent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "idemowner@example.com")
	o, err := svc.CreateOrg(ctx, "idemorg", "Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	team, err := svc.CreateTeam(ctx, o.ID, "idemteam", "Team")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	dev := newUser(t, pool, "idemdev@example.com")
	if err := svc.AddMember(ctx, o.ID, dev, org.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := svc.AddTeamMember(ctx, team.ID, dev); err != nil {
		t.Fatalf("AddTeamMember (first): %v", err)
	}
	if err := svc.AddTeamMember(ctx, team.ID, dev); err != nil {
		t.Fatalf("AddTeamMember (duplicate): got %v, want nil (idempotent)", err)
	}
}
