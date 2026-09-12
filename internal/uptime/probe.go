package uptime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// колонки status в таблице probes нет — web-слой вычисляет online/offline/
// revoked из last_seen_at/revoked_at; эти константы — источник истины набора.
const (
	ProbeStatusOnline  = "online"
	ProbeStatusOffline = "offline"
	ProbeStatusRevoked = "revoked"
)

var ProbeStatuses = []string{ProbeStatusOnline, ProbeStatusOffline, ProbeStatusRevoked}

type Probe struct {
	ID         int64
	OrgID      int64
	Region     string
	Name       string
	LastSeenAt *time.Time
	Revoked    bool
	CreatedAt  time.Time
}

func probeTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// возвращает сырой токен только здесь — единственный раз, когда вызывающий
// его видит; в БД сохраняется только sha256.
func (s *Service) CreateProbe(ctx context.Context, orgID int64, region, name string) (Probe, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Probe{}, "", fmt.Errorf("uptime: create probe: %w", err)
	}
	token := hex.EncodeToString(raw)

	p := Probe{OrgID: orgID, Region: region, Name: name}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO probes (org_id, region, name, token_hash)
		VALUES ($1,$2,$3,$4)
		RETURNING id, created_at`,
		orgID, region, name, probeTokenHash(token),
	).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return Probe{}, "", fmt.Errorf("uptime: create probe: %w", err)
	}
	return p, token, nil
}

// не идемпотентно — повторный отзыв уже отозванной или неизвестной пробы
// возвращает ErrNotFound.
func (s *Service) RevokeProbe(ctx context.Context, probeID int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE probes SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", probeID)
	if err != nil {
		return fmt.Errorf("uptime: revoke probe: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Probes(ctx context.Context, orgID int64) ([]Probe, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, region, name, last_seen_at, revoked_at IS NOT NULL, created_at
		FROM probes WHERE org_id = $1 ORDER BY id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("uptime: probes: %w", err)
	}
	defer rows.Close()
	var out []Probe
	for rows.Next() {
		var p Probe
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Region, &p.Name, &p.LastSeenAt, &p.Revoked, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("uptime: probes: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// отозванная проба трактуется как не найденная — отозванный токен сразу
// перестаёт аутентифицировать.
func (s *Service) ProbeByToken(ctx context.Context, token string) (Probe, error) {
	var p Probe
	err := s.pool.QueryRow(ctx, `
		SELECT id, org_id, region, name, last_seen_at, created_at
		FROM probes WHERE token_hash = $1 AND revoked_at IS NULL`, probeTokenHash(token),
	).Scan(&p.ID, &p.OrgID, &p.Region, &p.Name, &p.LastSeenAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Probe{}, ErrNotFound
	}
	if err != nil {
		return Probe{}, fmt.Errorf("uptime: probe by token: %w", err)
	}
	return p, nil
}

func (s *Service) TouchProbe(ctx context.Context, probeID int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE probes SET last_seen_at = now() WHERE id = $1", probeID)
	if err != nil {
		return fmt.Errorf("uptime: touch probe: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// встроенный регион — тот, что реально арендует раннер (s.localRegion()), не
// литерал "local": иначе монитор назначили бы на регион, где его не проверяют.
func (s *Service) Regions(ctx context.Context, orgID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT DISTINCT region FROM probes WHERE org_id = $1 AND revoked_at IS NULL", orgID)
	if err != nil {
		return nil, fmt.Errorf("uptime: regions: %w", err)
	}
	defer rows.Close()
	local := s.localRegion()
	seen := map[string]bool{local: true}
	out := []string{local}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("uptime: regions: %w", err)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: regions: %w", err)
	}
	sort.Strings(out)
	return out, nil
}
