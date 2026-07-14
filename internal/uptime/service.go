package uptime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

var (
	ErrNotFound       = errors.New("uptime: not found")
	ErrInvalidMonitor = errors.New("uptime: invalid monitor")
)

// Service — CRUD над мониторами доступности поверх PostgreSQL.
type Service struct {
	pool *pgxpool.Pool

	// LocalRegion — как на самом деле НАЗЫВАЕТСЯ регион встроенной пробы в
	// этой инсталляции: тот, которым Runner помечает свои проверки и который
	// он лизит (cmd/gotcha: cfg.LocalRegion, GOTCHA_LOCAL_REGION). Пустое
	// значение — DefaultRegion ("local"). Хардкодить "local" нельзя: Regions
	// предлагает этот список в форме монитора, и монитор, назначенный в
	// регион, который никто не лизит, не будет проверяться НИКОГДА (см.
	// localRegion).
	LocalRegion string
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// localRegion — имя встроенного региона: LocalRegion, а если не задано —
// DefaultRegion.
func (s *Service) localRegion() string {
	if s.LocalRegion == "" {
		return DefaultRegion
	}
	return s.LocalRegion
}

// generateHeartbeatToken — 32 случайных байта в hex (64 символа).
func generateHeartbeatToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// checkChannelsBelongToProject проверяет, что все channelIDs — каналы
// проекта projectID; иначе ErrInvalidMonitor.
func checkChannelsBelongToProject(ctx context.Context, tx pgx.Tx, projectID int64, channelIDs []int64) error {
	if len(channelIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx,
		"SELECT id FROM alert_channels WHERE project_id = $1 AND id = ANY($2)",
		projectID, channelIDs)
	if err != nil {
		return fmt.Errorf("uptime: check channels: %w", err)
	}
	defer rows.Close()
	found := make(map[int64]bool, len(channelIDs))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("uptime: check channels: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("uptime: check channels: %w", err)
	}
	for _, id := range channelIDs {
		if !found[id] {
			return fmt.Errorf("%w: channel %d does not belong to project %d", ErrInvalidMonitor, id, projectID)
		}
	}
	return nil
}

func insertRegions(ctx context.Context, tx pgx.Tx, monitorID int64, regions []string) error {
	for _, r := range regions {
		if _, err := tx.Exec(ctx,
			"INSERT INTO monitor_regions (monitor_id, region) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			monitorID, r); err != nil {
			return fmt.Errorf("uptime: insert region: %w", err)
		}
	}
	return nil
}

func insertChannels(ctx context.Context, tx pgx.Tx, monitorID int64, channelIDs []int64) error {
	for _, id := range channelIDs {
		if _, err := tx.Exec(ctx,
			"INSERT INTO monitor_channels (monitor_id, channel_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			monitorID, id); err != nil {
			return fmt.Errorf("uptime: insert channel: %w", err)
		}
	}
	return nil
}

// Create создаёт монитор вместе с регионами и каналами в одной транзакции.
// Пустые regions превращаются в ["local"]. Для kind=heartbeat генерирует
// уникальный heartbeat_token.
func (s *Service) Create(ctx context.Context, m Monitor, regions []string, channelIDs []int64) (Monitor, error) {
	if err := validateMonitor(m, regions); err != nil {
		return Monitor{}, err
	}
	if len(regions) == 0 {
		regions = []string{"local"}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Monitor{}, fmt.Errorf("uptime: create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkChannelsBelongToProject(ctx, tx, m.ProjectID, channelIDs); err != nil {
		return Monitor{}, err
	}

	if m.Kind == KindHeartbeat {
		token, err := generateHeartbeatToken()
		if err != nil {
			return Monitor{}, fmt.Errorf("uptime: create: %w", err)
		}
		m.HeartbeatToken = token
	} else {
		m.HeartbeatToken = ""
	}
	var heartbeatToken *string
	if m.HeartbeatToken != "" {
		heartbeatToken = &m.HeartbeatToken
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO monitors (project_id, name, kind, enabled, interval_seconds, timeout_seconds,
			config, fail_threshold, recovery_threshold, consensus, remind_every_minutes,
			ssl_alert_days, heartbeat_token)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id, created_at`,
		m.ProjectID, m.Name, string(m.Kind), m.Enabled, m.IntervalSeconds, m.TimeoutSeconds,
		m.Config, m.FailThreshold, m.RecoveryThreshold, string(m.Consensus), m.RemindEveryMinutes,
		m.SSLAlertDays, heartbeatToken,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return Monitor{}, fmt.Errorf("uptime: create: %w", err)
	}

	if err := insertRegions(ctx, tx, m.ID, regions); err != nil {
		return Monitor{}, err
	}
	if err := insertChannels(ctx, tx, m.ID, channelIDs); err != nil {
		return Monitor{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Monitor{}, fmt.Errorf("uptime: create: %w", err)
	}
	m.Regions = regions
	m.ChannelIDs = channelIDs
	return m, nil
}

// Update обновляет монитор и заменяет его regions/channels. kind и
// heartbeat_token монитора не меняются, даже если m содержит другие
// значения — они читаются из БД перед валидацией и записью.
func (s *Service) Update(ctx context.Context, m Monitor, regions []string, channelIDs []int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind Kind
	var projectID int64
	err = tx.QueryRow(ctx, "SELECT kind, project_id FROM monitors WHERE id = $1", m.ID).Scan(&kind, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	m.Kind = kind
	m.ProjectID = projectID

	if err := validateMonitor(m, regions); err != nil {
		return err
	}
	if len(regions) == 0 {
		regions = []string{"local"}
	}

	if err := checkChannelsBelongToProject(ctx, tx, m.ProjectID, channelIDs); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE monitors SET name=$2, enabled=$3, interval_seconds=$4, timeout_seconds=$5,
			config=$6, fail_threshold=$7, recovery_threshold=$8, consensus=$9,
			remind_every_minutes=$10, ssl_alert_days=$11
		WHERE id = $1`,
		m.ID, m.Name, m.Enabled, m.IntervalSeconds, m.TimeoutSeconds, m.Config,
		m.FailThreshold, m.RecoveryThreshold, string(m.Consensus), m.RemindEveryMinutes, m.SSLAlertDays)
	if err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if _, err := tx.Exec(ctx, "DELETE FROM monitor_regions WHERE monitor_id = $1", m.ID); err != nil {
		return fmt.Errorf("uptime: update regions: %w", err)
	}
	if err := insertRegions(ctx, tx, m.ID, regions); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, "DELETE FROM monitor_channels WHERE monitor_id = $1", m.ID); err != nil {
		return fmt.Errorf("uptime: update channels: %w", err)
	}
	if err := insertChannels(ctx, tx, m.ID, channelIDs); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("uptime: update: %w", err)
	}
	return nil
}

// Delete удаляет монитор. Каскадом (FK ON DELETE CASCADE) удаляются его
// regions, channels, state, инциденты и т.д.
func (s *Service) Delete(ctx context.Context, monitorID int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM monitors WHERE id = $1", monitorID)
	if err != nil {
		return fmt.Errorf("uptime: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func regionsOf(ctx context.Context, pool *pgxpool.Pool, monitorID int64) ([]string, error) {
	rows, err := pool.Query(ctx,
		"SELECT region FROM monitor_regions WHERE monitor_id = $1 ORDER BY region", monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: regions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("uptime: regions: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func channelIDsOf(ctx context.Context, pool *pgxpool.Pool, monitorID int64) ([]int64, error) {
	rows, err := pool.Query(ctx,
		"SELECT channel_id FROM monitor_channels WHERE monitor_id = $1 ORDER BY channel_id", monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: channels: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("uptime: channels: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func scanMonitor(row pgx.Row, m *Monitor) error {
	var heartbeatToken *string
	if err := row.Scan(&m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds, &m.TimeoutSeconds,
		&m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus, &m.RemindEveryMinutes,
		&m.SSLAlertDays, &m.SSLExpiresAt, &heartbeatToken, &m.LastBeatAt, &m.CreatedAt); err != nil {
		return err
	}
	if heartbeatToken != nil {
		m.HeartbeatToken = *heartbeatToken
	}
	return nil
}

const monitorColumns = `project_id, name, kind, enabled, interval_seconds, timeout_seconds, config,
	fail_threshold, recovery_threshold, consensus, remind_every_minutes, ssl_alert_days,
	ssl_expires_at, heartbeat_token, last_beat_at, created_at`

// Get возвращает монитор вместе с его regions и channels.
func (s *Service) Get(ctx context.Context, monitorID int64) (Monitor, error) {
	m := Monitor{ID: monitorID}
	row := s.pool.QueryRow(ctx, "SELECT "+monitorColumns+" FROM monitors WHERE id = $1", monitorID)
	if err := scanMonitor(row, &m); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Monitor{}, ErrNotFound
		}
		return Monitor{}, fmt.Errorf("uptime: get: %w", err)
	}

	regions, err := regionsOf(ctx, s.pool, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	m.Regions = regions

	channelIDs, err := channelIDsOf(ctx, s.pool, monitorID)
	if err != nil {
		return Monitor{}, err
	}
	m.ChannelIDs = channelIDs

	return m, nil
}

// List возвращает мониторы проекта, отсортированные по name, вместе с их
// regions (ChannelIDs не заполняются — см. Get).
func (s *Service) List(ctx context.Context, projectID int64) ([]Monitor, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, "+monitorColumns+" FROM monitors WHERE project_id = $1 ORDER BY name", projectID)
	if err != nil {
		return nil, fmt.Errorf("uptime: list: %w", err)
	}
	var out []Monitor
	for rows.Next() {
		var m Monitor
		var heartbeatToken *string
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds,
			&m.TimeoutSeconds, &m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus,
			&m.RemindEveryMinutes, &m.SSLAlertDays, &m.SSLExpiresAt, &heartbeatToken,
			&m.LastBeatAt, &m.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("uptime: list: %w", err)
		}
		if heartbeatToken != nil {
			m.HeartbeatToken = *heartbeatToken
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: list: %w", err)
	}

	for i := range out {
		regions, err := regionsOf(ctx, s.pool, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Regions = regions
	}
	return out, nil
}

// SetEnabled включает/выключает монитор.
func (s *Service) SetEnabled(ctx context.Context, monitorID int64, enabled bool) error {
	tag, err := s.pool.Exec(ctx, "UPDATE monitors SET enabled = $2 WHERE id = $1", monitorID, enabled)
	if err != nil {
		return fmt.Errorf("uptime: set enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ByHeartbeatToken ищет монитор kind=heartbeat по его токену — используется
// эндпоинтом приёма heartbeat-пингов.
func (s *Service) ByHeartbeatToken(ctx context.Context, token string) (Monitor, error) {
	var id int64
	err := s.pool.QueryRow(ctx, "SELECT id FROM monitors WHERE heartbeat_token = $1", token).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Monitor{}, ErrNotFound
	}
	if err != nil {
		return Monitor{}, fmt.Errorf("uptime: by heartbeat token: %w", err)
	}
	return s.Get(ctx, id)
}

// SetSSLExpiry records the certificate expiry observed by an https check.
// A no-op (besides the write itself) when expires equals the value already
// stored. When it differs and is LATER than the stored one, ssl_alerted_days
// is cleared — a later expiry means a new certificate was issued, so any
// "N days left" alerts already sent for the old one no longer apply. The
// comparison and the clear happen in a single UPDATE so a concurrent caller
// can't observe (or race) a half-applied state.
func (s *Service) SetSSLExpiry(ctx context.Context, monitorID int64, expires time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE monitors SET
			ssl_expires_at = $2,
			ssl_alerted_days = CASE
				WHEN ssl_expires_at IS NOT NULL AND $2 > ssl_expires_at THEN '{}'::int[]
				ELSE ssl_alerted_days
			END
		WHERE id = $1`,
		monitorID, expires)
	if err != nil {
		return fmt.Errorf("uptime: set ssl expiry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MonitorChannels returns monitorID's own delivery channels — the ones
// linked via monitor_channels — enabled only, ordered by id. An empty
// (nil) result is not an error: it means the monitor has no channels of
// its own, and callers (see OutboxNotifier.Notify) fall back to the
// project's channels. internal/uptime importing internal/alert here does
// not create an import cycle: alert never imports uptime.
func (s *Service) MonitorChannels(ctx context.Context, monitorID int64) ([]alert.Channel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.project_id, c.kind, c.enabled, c.target, c.secret
		FROM monitor_channels mc
		JOIN alert_channels c ON c.id = mc.channel_id
		WHERE mc.monitor_id = $1 AND c.enabled
		ORDER BY c.id`, monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: monitor channels: %w", err)
	}
	defer rows.Close()
	var out []alert.Channel
	for rows.Next() {
		var c alert.Channel
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Kind, &c.Enabled, &c.Target, &c.Secret); err != nil {
			return nil, fmt.Errorf("uptime: monitor channels: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TouchHeartbeat records that a heartbeat monitor just received a ping,
// setting last_beat_at = now(). Used by the public heartbeat endpoint
// (internal/web/heartbeat.go); the missed-ping watchdog (plan 3) reads
// last_beat_at to detect a monitor that stopped pinging
// (last_beat_at + grace < now()).
func (s *Service) TouchHeartbeat(ctx context.Context, monitorID int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE monitors SET last_beat_at = now() WHERE id = $1", monitorID)
	if err != nil {
		return fmt.Errorf("uptime: touch heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
