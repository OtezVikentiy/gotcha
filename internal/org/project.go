package org

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Project struct {
	ID       int64
	OrgID    int64
	Slug     string
	Name     string
	Platform string

	// Настройки производительности (этап 3): доля семплируемых транзакций
	// (0 — не принимать ни одной, 1 — все), порог Apdex в миллисекундах и
	// конфиг детекторов (JSON; читают детекторы, ingest его не трактует).
	TransactionSampleRate float64
	ApdexThresholdMS      int32
	PerfDetectorConfig    string

	// Конфиг детектора регрессий (этап 4): JSON порогов роста p95/p75 над
	// скользящей базой (ровно ключи trace.RegressionConfigFromJSON). Это
	// ОТДЕЛЬНЫЙ механизм от PerfDetectorConfig (N+1/медленные запросы этапа 3).
	PerfRegressionConfig string
}

// projectColumns — общий список колонок для всех SELECT'ов проекта: любой
// прочитанный Project приезжает целиком, чтобы вызывающий не получил
// TransactionSampleRate=0 (то есть «не семплировать вообще») из-за того, что
// конкретный запрос забыл колонку.
const projectColumns = "id, org_id, slug, name, platform, " +
	"transaction_sample_rate, apdex_threshold_ms, perf_detector_config, perf_regression_config"

// scanProject читает строку в порядке projectColumns (с префиксом таблицы или
// без — порядок один и тот же).
func scanProject(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.OrgID, &p.Slug, &p.Name, &p.Platform,
		&p.TransactionSampleRate, &p.ApdexThresholdMS, &p.PerfDetectorConfig, &p.PerfRegressionConfig)
	return p, err
}

// CreateProject создаёт проект в организации.
func (s *Service) CreateProject(ctx context.Context, orgID int64, slug, name, platform string) (Project, error) {
	if !validSlug(slug) {
		return Project{}, ErrInvalidSlug
	}
	// RETURNING всех колонок: у проекта есть поля со значениями по умолчанию из
	// БД (transaction_sample_rate и т.д.), и созданный Project обязан приехать
	// с ними, а не с нулями.
	p, err := scanProject(s.pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name, platform) VALUES ($1, $2, $3, $4) RETURNING "+projectColumns,
		orgID, slug, name, platform))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Project{}, ErrSlugTaken
	}
	if err != nil {
		return Project{}, fmt.Errorf("org: create project: %w", err)
	}
	return p, nil
}

// AttachTeam даёт команде доступ к проекту.
func (s *Service) AttachTeam(ctx context.Context, projectID, teamID int64) error {
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO project_teams (project_id, team_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		projectID, teamID); err != nil {
		return fmt.Errorf("org: attach team: %w", err)
	}
	return nil
}

// DetachTeam убирает доступ команды к проекту. Идемпотентно — как и
// AttachTeam, отсутствие связи не считается ошибкой: повторный вызов или
// detach никогда не существовавшей связи просто ничего не делает.
func (s *Service) DetachTeam(ctx context.Context, projectID, teamID int64) error {
	if _, err := s.pool.Exec(ctx,
		"DELETE FROM project_teams WHERE project_id = $1 AND team_id = $2",
		projectID, teamID); err != nil {
		return fmt.Errorf("org: detach team: %w", err)
	}
	return nil
}

// RenameProject меняет отображаемое имя проекта. Пустое имя → ErrInvalidName
// (до похода в БД); несуществующий проект → ErrNotFound.
func (s *Service) RenameProject(ctx context.Context, projectID int64, name string) error {
	if name == "" {
		return ErrInvalidName
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE projects SET name = $2 WHERE id = $1", projectID, name)
	if err != nil {
		return fmt.Errorf("org: rename project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePerfSettings пишет настройки производительности проекта одним UPDATE:
// долю семплируемых транзакций, порог Apdex (мс) и JSON конфига детекторов
// (ровно те ключи, что читает trace.ConfigFromJSON). Значения уже провалидированы
// вызывающим (форма настроек); несуществующий проект → ErrNotFound.
func (s *Service) UpdatePerfSettings(ctx context.Context, projectID int64, sampleRate float64, apdexMS int32, detectorConfigJSON string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE projects SET transaction_sample_rate = $1, apdex_threshold_ms = $2, perf_detector_config = $3 WHERE id = $4",
		sampleRate, apdexMS, detectorConfigJSON, projectID)
	if err != nil {
		return fmt.Errorf("org: update perf settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateRegressionConfig пишет JSON конфига детектора регрессий одним UPDATE
// (ровно те ключи, что читает trace.RegressionConfigFromJSON). Значения уже
// провалидированы вызывающим (форма настроек «Регрессии»); несуществующий
// проект → ErrNotFound. Отдельный метод от UpdatePerfSettings: это другая
// колонка и другой механизм (см. Project.PerfRegressionConfig).
func (s *Service) UpdateRegressionConfig(ctx context.Context, projectID int64, configJSON string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE projects SET perf_regression_config = $1 WHERE id = $2",
		configJSON, projectID)
	if err != nil {
		return fmt.Errorf("org: update regression config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProject удаляет проект. FK на projects (project_keys, monitors,
// status_pages, maintenance_windows, issues, alert_rules и т.д.) объявлены
// ON DELETE CASCADE в PG, поэтому зависимые записи снимаются автоматически и
// осиротевших мониторов не остаётся. Телеметрию в ClickHouse каскад НЕ трогает —
// её чистит вызывающий (см. telemetry.Purger в web-слое). Несуществующий
// projectID → ErrNotFound.
func (s *Service) DeleteProject(ctx context.Context, projectID int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM projects WHERE id = $1", projectID)
	if err != nil {
		return fmt.Errorf("org: delete project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// accessCondition: owner/admin видят все проекты организации,
// member — проекты команд, в которых состоит.
const accessCondition = `
	EXISTS (
		SELECT 1 FROM org_members m
		WHERE m.org_id = p.org_id AND m.user_id = $1 AND m.role IN ('owner','admin')
	) OR EXISTS (
		SELECT 1 FROM project_teams pt
		JOIN team_members tm ON tm.team_id = pt.team_id
		WHERE pt.project_id = p.id AND tm.user_id = $1
	)`

// ProjectsForUser возвращает проекты, доступные пользователю.
func (s *Service) ProjectsForUser(ctx context.Context, userID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+projectColumns+" FROM projects p WHERE "+
			accessCondition+" ORDER BY p.id", userID)
	if err != nil {
		return nil, fmt.Errorf("org: projects for user: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("org: projects for user: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject возвращает проект со всеми настройками; несуществующий id →
// ErrNotFound. Ingest читает его на горячем пути (через кеш, см.
// ingest.ProjectCache) ради transaction_sample_rate.
func (s *Service) GetProject(ctx context.Context, projectID int64) (Project, error) {
	p, err := scanProject(s.pool.QueryRow(ctx,
		"SELECT "+projectColumns+" FROM projects WHERE id = $1", projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("org: get project: %w", err)
	}
	return p, nil
}

// ProjectsOf возвращает все проекты организации, отсортированные по name —
// нужен странице команд (план 5, задача 3) для select привязки проекта к
// команде: там важны все проекты организации, а не только доступные
// конкретному пользователю (в отличие от ProjectsForUser).
func (s *Service) ProjectsOf(ctx context.Context, orgID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+projectColumns+" FROM projects WHERE org_id = $1 ORDER BY name", orgID)
	if err != nil {
		return nil, fmt.Errorf("org: projects of: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("org: projects of: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectOrg возвращает orgID проекта; несуществующий projectID → ErrNotFound.
// Нужен странице issue (план 4), чтобы от issue.ProjectID дойти до
// MembersOf(orgID) для assign-select.
func (s *Service) ProjectOrg(ctx context.Context, projectID int64) (int64, error) {
	var orgID int64
	err := s.pool.QueryRow(ctx, "SELECT org_id FROM projects WHERE id = $1", projectID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("org: project org: %w", err)
	}
	return orgID, nil
}

// CanAccessProject — точечная проверка того же правила доступа.
func (s *Service) CanAccessProject(ctx context.Context, userID, projectID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		"SELECT "+accessCondition+" FROM projects p WHERE p.id = $2",
		userID, projectID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("org: can access project: %w", err)
	}
	return ok, nil
}
