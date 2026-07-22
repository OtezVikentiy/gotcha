// Package alert — правила алертинга (new_issue/regression/spike) и каналы
// доставки (email/webhook/telegram) на уровне проекта.
package alert

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/mail"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

// Kinds правил — совпадают с CHECK-ограничением alert_rules.kind.
const (
	KindNewIssue   = "new_issue"
	KindRegression = "regression"
	KindSpike      = "spike"
)

// Kinds каналов — совпадают с CHECK-ограничением alert_channels.kind.
const (
	ChannelEmail    = "email"
	ChannelWebhook  = "webhook"
	ChannelTelegram = "telegram"
)

var (
	ErrNotFound       = errors.New("alert: not found")
	ErrInvalidRule    = errors.New("alert: invalid rule")
	ErrInvalidChannel = errors.New("alert: invalid channel")
)

// Rule — правило алертинга проекта.
type Rule struct {
	ID              int64
	ProjectID       int64
	Kind            string
	Enabled         bool
	Threshold       int
	WindowMinutes   int
	ThrottleMinutes int
}

// Channel — канал доставки уведомлений проекта.
type Channel struct {
	ID        int64
	ProjectID int64
	Kind      string
	Enabled   bool
	Target    string
	Secret    string
}

// Service — CRUD над правилами и каналами алертинга.
type Service struct {
	pool         *pgxpool.Pool
	secretKey    [32]byte
	secretKeySet bool
}

// SetSecretKey включает шифрование секретов каналов (Telegram bot-токен, HMAC-
// ключ webhook) at-rest тем же мастер-ключом, что и SSO client_secret. Пустой
// ключ (dev) → секреты хранятся plaintext (openSecret/Open распознаёт это по
// отсутствию префикса "enc:"). Ставится из main.go.
func (s *Service) SetSecretKey(raw string) {
	if raw == "" {
		return
	}
	s.secretKey = sha256.Sum256([]byte(raw))
	s.secretKeySet = true
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func validRuleKind(kind string) bool {
	switch kind {
	case KindNewIssue, KindRegression, KindSpike:
		return true
	default:
		return false
	}
}

// validateRule проверяет правило до похода в БД: kind должен быть одним из
// известных, a spike дополнительно требует Threshold>0 и WindowMinutes>0 —
// иначе правило никогда не сработает. ThrottleMinutes >= 0 required (0 means no throttle).
func validateRule(r Rule) error {
	if !validRuleKind(r.Kind) {
		return ErrInvalidRule
	}
	if r.Kind == KindSpike && (r.Threshold <= 0 || r.WindowMinutes <= 0) {
		return ErrInvalidRule
	}
	if r.ThrottleMinutes < 0 {
		return ErrInvalidRule
	}
	return nil
}

// validateChannel проверяет канал до похода в БД: email — валидный адрес,
// webhook — http(s) URL, telegram — непустые chat_id (Target) и bot token
// (Secret).
func validateChannel(c Channel) error {
	switch c.Kind {
	case ChannelEmail:
		if _, err := mail.ParseAddress(c.Target); err != nil {
			return ErrInvalidChannel
		}
	case ChannelWebhook:
		u, err := url.Parse(c.Target)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return ErrInvalidChannel
		}
	case ChannelTelegram:
		if c.Target == "" || c.Secret == "" {
			return ErrInvalidChannel
		}
	default:
		return ErrInvalidChannel
	}
	return nil
}

// Rules возвращает правила проекта, отсортированные по kind.
func (s *Service) Rules(ctx context.Context, projectID int64) ([]Rule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, kind, enabled, threshold, window_minutes, throttle_minutes
		FROM alert_rules WHERE project_id = $1 ORDER BY kind`, projectID)
	if err != nil {
		return nil, fmt.Errorf("alert: rules: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Kind, &r.Enabled,
			&r.Threshold, &r.WindowMinutes, &r.ThrottleMinutes); err != nil {
			return nil, fmt.Errorf("alert: rules: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRule создаёт или обновляет правило проекта. UNIQUE(project_id, kind)
// — повторный вызов с тем же kind обновляет существующее правило.
func (s *Service) UpsertRule(ctx context.Context, r Rule) (int64, error) {
	if err := validateRule(r); err != nil {
		return 0, err
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO alert_rules (project_id, kind, enabled, threshold, window_minutes, throttle_minutes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project_id, kind) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			threshold = EXCLUDED.threshold,
			window_minutes = EXCLUDED.window_minutes,
			throttle_minutes = EXCLUDED.throttle_minutes
		RETURNING id`,
		r.ProjectID, r.Kind, r.Enabled, r.Threshold, r.WindowMinutes, r.ThrottleMinutes).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("alert: upsert rule: %w", err)
	}
	return id, nil
}

// DeleteRule удаляет правило по id.
func (s *Service) DeleteRule(ctx context.Context, ruleID int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM alert_rules WHERE id = $1", ruleID)
	if err != nil {
		return fmt.Errorf("alert: delete rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Channels возвращает каналы доставки проекта, отсортированные по id.
func (s *Service) Channels(ctx context.Context, projectID int64) ([]Channel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, kind, enabled, target, secret
		FROM alert_channels WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("alert: channels: %w", err)
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Kind, &c.Enabled, &c.Target, &c.Secret); err != nil {
			return nil, fmt.Errorf("alert: channels: %w", err)
		}
		// Расшифровываем секрет, если задан мастер-ключ (legacy plaintext без
		// префикса "enc:" Open вернёт как есть — совместимость со старыми записями).
		if s.secretKeySet {
			secret, err := secretbox.Open(s.secretKey, c.Secret)
			if err != nil {
				return nil, fmt.Errorf("alert: open channel secret: %w", err)
			}
			c.Secret = secret
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateChannel создаёт канал доставки проекта.
func (s *Service) CreateChannel(ctx context.Context, c Channel) (int64, error) {
	if err := validateChannel(c); err != nil {
		return 0, err
	}
	// Шифруем секрет at-rest, если задан мастер-ключ (иначе plaintext, как для
	// пустого ключа — читатель распознаёт по отсутствию префикса "enc:").
	storedSecret := c.Secret
	if s.secretKeySet {
		sealed, err := secretbox.Seal(s.secretKey, c.Secret)
		if err != nil {
			return 0, fmt.Errorf("alert: seal channel secret: %w", err)
		}
		storedSecret = sealed
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO alert_channels (project_id, kind, enabled, target, secret)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		c.ProjectID, c.Kind, c.Enabled, c.Target, storedSecret).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("alert: create channel: %w", err)
	}
	return id, nil
}

// DeleteChannel удаляет канал по id. Каскадом удаляет и его записи в outbox.
func (s *Service) DeleteChannel(ctx context.Context, channelID int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM alert_channels WHERE id = $1", channelID)
	if err != nil {
		return fmt.Errorf("alert: delete channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// EnsureDefaultRules заводит правила new_issue и regression (enabled,
// throttle 30 минут) для нового проекта, если их ещё нет. Идемпотентна:
// UNIQUE(project_id, kind) + ON CONFLICT DO NOTHING не трогает уже
// настроенные вручную правила. Вызывается из web-слоя там, где создаётся
// проект (онбординг, настройки проекта) — не из org.CreateProject, чтобы
// не тянуть зависимость org → alert.
func (s *Service) EnsureDefaultRules(ctx context.Context, projectID int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO alert_rules (project_id, kind, enabled, throttle_minutes)
		VALUES ($1, $2, true, 30), ($1, $3, true, 30)
		ON CONFLICT (project_id, kind) DO NOTHING`,
		projectID, KindNewIssue, KindRegression)
	if err != nil {
		return fmt.Errorf("alert: ensure default rules: %w", err)
	}
	return nil
}
