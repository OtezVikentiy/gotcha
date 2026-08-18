// Package deploy — реестр деплоев проекта (таблица deployments): CI пушит
// событие выкладки (версия, окружение, время), а UI рисует по ним вертикальные
// маркеры на графиках и список деплоев, плюс привязывает регрессии к ближайшему
// предшествующему деплою. Зеркало internal/host по форме стора.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deployment — строка таблицы deployments: одна выкладка проекта. DeployedAt —
// момент самой выкладки (его шлёт CI, по нему строится маркер), CreatedAt —
// момент приёма записи стором.
type Deployment struct {
	ID          int64
	ProjectID   int64
	Version     string
	Environment string
	URL         string
	Changelog   string
	DeployedAt  time.Time
	CreatedAt   time.Time
}

// Капы длины на входе Record — защита горячего пути приёма от разросшихся полей
// (CI вполне способен прислать многокилобайтный changelog). Обрезка по рунам,
// чтобы не рвать UTF-8 посередине символа.
const (
	maxVersion     = 512
	maxEnvironment = 128
	maxURL         = 2048
	maxChangelog   = 16384
)

// capStr — обрезка строки до n рун. Имя НЕ `cap`: тот шадовит builtin.
func capStr(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// deployColumns — порядок колонок для scanDeployment (аналог hostColumns).
const deployColumns = `id, project_id, version, environment, url, changelog, deployed_at, created_at`

func scanDeployment(row pgx.Row) (Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.ProjectID, &d.Version, &d.Environment, &d.URL, &d.Changelog, &d.DeployedAt, &d.CreatedAt)
	return d, err
}

// Store — CRUD поверх таблицы deployments.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Record вставляет одну выкладку и возвращает её с заполненными ID/CreatedAt.
// Пустой DeployedAt подставляется как now() (CI мог не прислать время). Поля
// капаются по длине защитно.
func (s *Store) Record(ctx context.Context, projectID int64, d Deployment) (Deployment, error) {
	if d.DeployedAt.IsZero() {
		d.DeployedAt = time.Now().UTC()
	}
	d.ProjectID = projectID
	d.Version = capStr(d.Version, maxVersion)
	d.Environment = capStr(d.Environment, maxEnvironment)
	d.URL = capStr(d.URL, maxURL)
	d.Changelog = capStr(d.Changelog, maxChangelog)
	row := s.pool.QueryRow(ctx,
		`INSERT INTO deployments (project_id, version, environment, deployed_at, url, changelog)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		projectID, d.Version, d.Environment, d.DeployedAt, d.URL, d.Changelog)
	if err := row.Scan(&d.ID, &d.CreatedAt); err != nil {
		return Deployment{}, fmt.Errorf("deploy: record: %w", err)
	}
	return d, nil
}

// List возвращает деплои проекта в окне [from, to), newest-first, не больше
// limit строк (limit <= 0 → без лишнего добора, берём 1000 защитно).
func (s *Store) List(ctx context.Context, projectID int64, from, to time.Time, limit int) ([]Deployment, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+deployColumns+" FROM deployments WHERE project_id = $1 AND deployed_at >= $2 AND deployed_at < $3 ORDER BY deployed_at DESC LIMIT $4",
		projectID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("deploy: list: %w", err)
	}
	defer rows.Close()
	return scanDeployments(rows)
}

// Recent возвращает последние limit деплоев проекта, newest-first (limit <= 0 →
// 100 защитно).
func (s *Store) Recent(ctx context.Context, projectID int64, limit int) ([]Deployment, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+deployColumns+" FROM deployments WHERE project_id = $1 ORDER BY deployed_at DESC LIMIT $2",
		projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("deploy: recent: %w", err)
	}
	defer rows.Close()
	return scanDeployments(rows)
}

// Nearest возвращает ближайший к before деплой ПРЕДШЕСТВУЮЩИЙ ему (deployed_at
// <= before), то есть тот, после которого начался интересующий момент. ok=false,
// если раньше before деплоев не было (привязка регрессии просто не рисуется).
func (s *Store) Nearest(ctx context.Context, projectID int64, before time.Time) (Deployment, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+deployColumns+" FROM deployments WHERE project_id = $1 AND deployed_at <= $2 ORDER BY deployed_at DESC LIMIT 1",
		projectID, before)
	d, err := scanDeployment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, false, nil
	}
	if err != nil {
		return Deployment{}, false, fmt.Errorf("deploy: nearest: %w", err)
	}
	return d, true, nil
}

func scanDeployments(rows pgx.Rows) ([]Deployment, error) {
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("deploy: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
