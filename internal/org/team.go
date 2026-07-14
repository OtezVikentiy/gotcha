package org

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Team struct {
	ID    int64
	OrgID int64
	Slug  string
	Name  string
}

// CreateTeam создаёт команду в организации.
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

// TeamsOf возвращает команды организации, отсортированные по name —
// стабильный порядок для страницы настроек.
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

// TeamOrg возвращает orgID команды; несуществующий teamID → ErrNotFound.
// Нужен веб-хендлерам команд (план 5, задача 3), чтобы от team ID дойти до
// requireOrgRole — маршруты команд авторизуются по организации команды, а не
// по самой команде.
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

// TeamMembers возвращает участников команды, отсортированных по email.
// Роль берётся из членства в организации (team_members сама роли не хранит —
// участие в команде без участия в организации невозможно, см. AddTeamMember).
func (s *Service) TeamMembers(ctx context.Context, teamID int64) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, m.role
		FROM team_members tm
		JOIN teams t ON t.id = tm.team_id
		JOIN users u ON u.id = tm.user_id
		JOIN org_members m ON m.org_id = t.org_id AND m.user_id = u.id
		WHERE tm.team_id = $1
		ORDER BY u.email`, teamID)
	if err != nil {
		return nil, fmt.Errorf("org: team members: %w", err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role); err != nil {
			return nil, fmt.Errorf("org: team members: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: team members: %w", err)
	}
	return out, nil
}

// TeamProjects возвращает проекты, к которым у команды есть доступ.
func (s *Service) TeamProjects(ctx context.Context, teamID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.org_id, p.slug, p.name, p.platform
		FROM project_teams pt
		JOIN projects p ON p.id = pt.project_id
		WHERE pt.team_id = $1
		ORDER BY p.id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("org: team projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Slug, &p.Name, &p.Platform); err != nil {
			return nil, fmt.Errorf("org: team projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: team projects: %w", err)
	}
	return out, nil
}

// RemoveTeamMember убирает участника из команды. Не идемпотентно (в отличие
// от AddTeamMember): 0 затронутых строк (юзер и так не состоял в команде) →
// ErrNotMember, тот же сентинел, что и у RemoveMember на уровне организации.
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

// AddTeamMember добавляет в команду участника её организации.
func (s *Service) AddTeamMember(ctx context.Context, teamID, userID int64) error {
	// Один запрос: вставка проходит только если userID — участник
	// организации, которой принадлежит команда.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO team_members (team_id, user_id)
		SELECT t.id, $2 FROM teams t
		JOIN org_members m ON m.org_id = t.org_id AND m.user_id = $2
		WHERE t.id = $1`,
		teamID, userID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Идемпотентность: пользователь уже в команде — считаем успехом.
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
