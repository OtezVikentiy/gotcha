package org

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Team struct {
	ID    int64
	OrgID int64
	Slug  string
	Name  string
}

func (s *Service) CreateTeam(ctx context.Context, orgID int64, slug, name string) (Team, error) {
	if !validSlug(slug) {
		return Team{}, ErrInvalidSlug
	}
	tm := Team{OrgID: orgID, Slug: slug, Name: name}
	err := s.pool.QueryRow(ctx,
		"INSERT INTO teams (org_id, slug, name) VALUES ($1, $2, $3) RETURNING id",
		orgID, slug, name).Scan(&tm.ID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Team{}, ErrSlugTaken
	}
	if err != nil {
		return Team{}, fmt.Errorf("org: create team: %w", err)
	}
	return tm, nil
}

func (s *Service) RenameTeam(ctx context.Context, orgID, teamID int64, name string) error {
	if strings.TrimSpace(name) == "" {
		return ErrInvalidName
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE teams SET name = $3 WHERE id = $2 AND org_id = $1", orgID, teamID, name)
	if err != nil {
		return fmt.Errorf("org: rename team: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) DeleteTeam(ctx context.Context, orgID, teamID int64) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM teams WHERE id = $2 AND org_id = $1", orgID, teamID)
	if err != nil {
		return fmt.Errorf("org: delete team: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) TeamsOf(ctx context.Context, orgID int64) ([]Team, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, org_id, slug, name FROM teams WHERE org_id = $1 ORDER BY name", orgID)
	if err != nil {
		return nil, fmt.Errorf("org: teams of: %w", err)
	}
	defer rows.Close()

	var out []Team
	for rows.Next() {
		var tm Team
		if err := rows.Scan(&tm.ID, &tm.OrgID, &tm.Slug, &tm.Name); err != nil {
			return nil, fmt.Errorf("org: teams of: %w", err)
		}
		out = append(out, tm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: teams of: %w", err)
	}
	return out, nil
}

func (s *Service) TeamOrg(ctx context.Context, teamID int64) (int64, error) {
	var orgID int64
	err := s.pool.QueryRow(ctx, "SELECT org_id FROM teams WHERE id = $1", teamID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("org: team org: %w", err)
	}
	return orgID, nil
}

func (s *Service) TeamMembers(ctx context.Context, teamID int64) ([]Member, error) {
	byTeam, err := s.TeamMembersOf(ctx, []int64{teamID})
	if err != nil {
		return nil, err
	}
	return byTeam[teamID], nil
}

func (s *Service) TeamMembersOf(ctx context.Context, teamIDs []int64) (map[int64][]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tm.team_id, u.id, u.email, m.role
		FROM team_members tm
		JOIN teams t ON t.id = tm.team_id
		JOIN users u ON u.id = tm.user_id
		JOIN org_members m ON m.org_id = t.org_id AND m.user_id = u.id
		WHERE tm.team_id = ANY($1)
		ORDER BY tm.team_id, u.email`, teamIDs)
	if err != nil {
		return nil, fmt.Errorf("org: team members: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]Member)
	for rows.Next() {
		var teamID int64
		var m Member
		if err := rows.Scan(&teamID, &m.UserID, &m.Email, &m.Role); err != nil {
			return nil, fmt.Errorf("org: team members: %w", err)
		}
		out[teamID] = append(out[teamID], m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: team members: %w", err)
	}
	return out, nil
}

func (s *Service) TeamProjects(ctx context.Context, teamID int64) ([]Project, error) {
	byTeam, err := s.TeamProjectsOf(ctx, []int64{teamID})
	if err != nil {
		return nil, err
	}
	return byTeam[teamID], nil
}

func (s *Service) TeamProjectsOf(ctx context.Context, teamIDs []int64) (map[int64][]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pt.team_id, p.id, p.org_id, p.slug, p.name, p.platform
		FROM project_teams pt
		JOIN projects p ON p.id = pt.project_id
		WHERE pt.team_id = ANY($1)
		ORDER BY pt.team_id, p.id`, teamIDs)
	if err != nil {
		return nil, fmt.Errorf("org: team projects: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]Project)
	for rows.Next() {
		var teamID int64
		var p Project
		if err := rows.Scan(&teamID, &p.ID, &p.OrgID, &p.Slug, &p.Name, &p.Platform); err != nil {
			return nil, fmt.Errorf("org: team projects: %w", err)
		}
		out[teamID] = append(out[teamID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: team projects: %w", err)
	}
	return out, nil
}

func (s *Service) RemoveTeamMember(ctx context.Context, teamID, userID int64) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM team_members WHERE team_id = $1 AND user_id = $2", teamID, userID)
	if err != nil {
		return fmt.Errorf("org: remove team member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

func (s *Service) AddTeamMember(ctx context.Context, teamID, userID int64) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO team_members (team_id, org_id, user_id)
		SELECT t.id, t.org_id, $2 FROM teams t
		JOIN org_members m ON m.org_id = t.org_id AND m.user_id = $2
		WHERE t.id = $1`,
		teamID, userID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("org: add team member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}
