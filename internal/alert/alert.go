package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

// Kinds правил — совпадают с CHECK-ограничением alert_rules.kind.
const (
	KindNewIssue   = notify.KindNewIssue
	KindRegression = notify.KindRegression
	KindSpike      = notify.KindSpike
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
	// Секрет зашифрован (настоящий enc:-ciphertext), но мастер-ключа нет —
	// расшифровать нечем, а отдавать ciphertext как живой секрет нельзя.
	ErrSecretBroken = errors.New("alert: channel secret is encrypted but no master key is set")
)

type Rule struct {
	ID              int64
	ProjectID       int64
	Kind            string
	Enabled         bool
	Threshold       int
	WindowMinutes   int
	ThrottleMinutes int
}

type Channel struct {
	ID        int64
	ProjectID int64
	Kind      string
	Enabled   bool
	Target    string
	Secret    string
	// Секрет не расшифровывается — канал остаётся в списке (не пропадает
	// молча), но Secret пуст и расшифровать его уже нечем.
	SecretBroken bool
	// Оператор подтвердил, что получатель внутри контура, — детали события
	// уходят и туда, где домен не определить (Telegram), см. DetailPolicy.
	Trusted bool
}

// Одно место на все нотифаеры — выключенный канал и канал со сломанным
// секретом одинаково недоступны для доставки, но по разным причинам.
func (c Channel) Deliverable() bool { return c.Enabled && !c.SecretBroken }

type Service struct {
	pool         *pgxpool.Pool
	ring         secretbox.Keyring
	secretKeySet bool

	// Пер-проектный потолок уведомлений (см. budget.go). budgetSet отличает
	// «оператор задал 0, значит выключил» от «не настраивалось, берём дефолт».
	budgetWindow time.Duration
	budgetLimit  int
	budgetSet    bool
}

// Не вызывается для dev-стендов — секреты остаются plaintext (Keyring.Open
// распознаёт это по отсутствию префикса "enc:").
func (s *Service) SetKeyring(ring secretbox.Keyring) {
	s.ring = ring
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

// spike дополнительно требует Threshold>0 и WindowMinutes>0 — иначе
// правило никогда не сработает.
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

// Email: только адрес, без отображаемого имени — mail.ParseAddress иначе
// пропустил бы «Name <addr>» как есть, а RCPT TO такое не принимает.
func normalizeChannelTarget(c Channel) Channel {
	c.Target = strings.TrimSpace(c.Target)
	if c.Kind == ChannelEmail {
		if a, err := mail.ParseAddress(c.Target); err == nil {
			c.Target = a.Address
		}
	}
	return c
}

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
		if c.Secret == "" {
			return ErrInvalidChannel
		}
		// chat_id — целое число (группы/супергруппы — отрицательное). Раньше
		// проверялась только непустота, и опечатка ловилась лишь в логе доставки.
		if _, err := strconv.ParseInt(c.Target, 10, 64); err != nil {
			return ErrInvalidChannel
		}
	default:
		return ErrInvalidChannel
	}
	return nil
}

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

// UNIQUE(project_id, kind) — повторный вызов с тем же kind обновляет
// существующее правило.
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

// Атомарно: валидация — целиком ДО первой записи, сами записи — в одной
// транзакции, иначе частичное применение при ошибке ловится посередине.
func (s *Service) UpsertRules(ctx context.Context, rules []Rule) error {
	for _, r := range rules {
		if err := validateRule(r); err != nil {
			return err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("alert: upsert rules: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, r := range rules {
		_, err := tx.Exec(ctx, `
			INSERT INTO alert_rules (project_id, kind, enabled, threshold, window_minutes, throttle_minutes)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (project_id, kind) DO UPDATE SET
				enabled = EXCLUDED.enabled,
				threshold = EXCLUDED.threshold,
				window_minutes = EXCLUDED.window_minutes,
				throttle_minutes = EXCLUDED.throttle_minutes`,
			r.ProjectID, r.Kind, r.Enabled, r.Threshold, r.WindowMinutes, r.ThrottleMinutes)
		if err != nil {
			return fmt.Errorf("alert: upsert rules: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("alert: upsert rules: %w", err)
	}
	return nil
}

func (s *Service) DeleteRule(ctx context.Context, projectID, ruleID int64) error {
	// project_id в условии — см. DeleteChannel.
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM alert_rules WHERE id = $1 AND project_id = $2", ruleID, projectID)
	if err != nil {
		return fmt.Errorf("alert: delete rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Channels(ctx context.Context, projectID int64) ([]Channel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, kind, enabled, target, secret, trusted
		FROM alert_channels WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("alert: channels: %w", err)
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Kind, &c.Enabled, &c.Target, &c.Secret, &c.Trusted); err != nil {
			return nil, fmt.Errorf("alert: channels: %w", err)
		}
		// Расшифровываем секрет, если задан мастер-ключ (legacy plaintext без
		// префикса "enc:" Open вернёт как есть — совместимость со старыми записями).
		if s.secretKeySet {
			secret, err := s.ring.Open(c.Secret)
			if err != nil {
				// Деградируем поканально — один нерасшифруемый секрет не должен убивать
				// доставку по остальным каналам, и канал не должен исчезать из списка.
				slog.Error("alert: channel secret cannot be decrypted",
					"channel_id", c.ID, "project_id", c.ProjectID, "kind", c.Kind, "error", err)
				c.Secret = ""
				c.SecretBroken = true
				out = append(out, c)
				continue
			}
			c.Secret = secret
		} else if secretbox.IsEncrypted(c.Secret) {
			// Ciphertext есть, а мастер-ключа нет — отдать as-is значило бы подсунуть
			// нотифаеру enc:base64... вместо токена; помечаем сломанным, как выше.
			slog.Error("alert: channel secret is encrypted but no master key is set",
				"channel_id", c.ID, "project_id", c.ProjectID, "kind", c.Kind)
			c.Secret = ""
			c.SecretBroken = true
			out = append(out, c)
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) CreateChannel(ctx context.Context, c Channel) (int64, error) {
	if err := validateChannel(c); err != nil {
		return 0, err
	}
	c = normalizeChannelTarget(c)
	// Шифруем секрет at-rest, если задан мастер-ключ (иначе plaintext, как для
	// пустого ключа — читатель распознаёт по отсутствию префикса "enc:").
	storedSecret := c.Secret
	if s.secretKeySet {
		sealed, err := s.ring.Seal(c.Secret)
		if err != nil {
			return 0, fmt.Errorf("alert: seal channel secret: %w", err)
		}
		storedSecret = sealed
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO alert_channels (project_id, kind, enabled, target, secret, trusted)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		c.ProjectID, c.Kind, c.Enabled, c.Target, storedSecret, c.Trusted).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("alert: create channel: %w", err)
	}
	return id, nil
}

// Секрет достаётся здесь, в момент отправки, не хранится в outbox.payload —
// иначе он лежал бы в очереди открытым текстом всё окно хранения.
func (s *Service) ChannelSecret(ctx context.Context, channelID int64) (string, error) {
	var secret string
	err := s.pool.QueryRow(ctx,
		"SELECT secret FROM alert_channels WHERE id = $1", channelID).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("alert: channel secret: %w", err)
	}
	if !s.secretKeySet {
		if secretbox.IsEncrypted(secret) {
			// Ciphertext без ключа для расшифровки — отдать его как secret значило бы
			// отправить enc:base64... нотифаеру вместо токена.
			return "", fmt.Errorf("alert: channel %d secret is encrypted: %w", channelID, ErrSecretBroken)
		}
		return secret, nil
	}
	open, err := s.ring.Open(secret)
	if err != nil {
		return "", fmt.Errorf("alert: channel %d secret cannot be decrypted: %w", channelID, err)
	}
	return open, nil
}

// Пустой Secret значит «оставить прежний» — секрет вводится вслепую и не
// возвращается в форму, иначе правка адреса требовала бы ввода токена заново.
func (s *Service) UpdateChannel(ctx context.Context, c Channel) error {
	if err := validateChannelForUpdate(c); err != nil {
		return err
	}
	c = normalizeChannelTarget(c)
	if c.Secret == "" {
		tag, err := s.pool.Exec(ctx, `
			UPDATE alert_channels SET target = $2, enabled = $3, trusted = $4
			WHERE id = $1 AND project_id = $5`, c.ID, c.Target, c.Enabled, c.Trusted, c.ProjectID)
		if err != nil {
			return fmt.Errorf("alert: update channel: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	}

	stored := c.Secret
	if s.secretKeySet {
		sealed, err := s.ring.Seal(c.Secret)
		if err != nil {
			return fmt.Errorf("alert: seal channel secret: %w", err)
		}
		stored = sealed
	}
	// project_id в условии — скоуп: без него владелец одного проекта мог бы
	// править канал соседнего.
	tag, err := s.pool.Exec(ctx, `
		UPDATE alert_channels SET target = $2, enabled = $3, secret = $4, trusted = $5
		WHERE id = $1 AND project_id = $6`, c.ID, c.Target, c.Enabled, stored, c.Trusted, c.ProjectID)
	if err != nil {
		return fmt.Errorf("alert: update channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Пустой секрет допустим — при изменении он означает «оставить прежний».
func validateChannelForUpdate(c Channel) error {
	probe := c
	if probe.Kind == ChannelTelegram && probe.Secret == "" {
		probe.Secret = "keep" // проверяем всё, кроме наличия секрета
	}
	return validateChannel(probe)
}

// Каскадом удаляет и его записи в outbox.
func (s *Service) DeleteChannel(ctx context.Context, projectID, channelID int64) error {
	// project_id в условии — тот же скоуп, что и у UpdateChannel: правило
	// должно жить в одном месте, а не полагаться на предпроверку хендлера.
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM alert_channels WHERE id = $1 AND project_id = $2", channelID, projectID)
	if err != nil {
		return fmt.Errorf("alert: delete channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Идемпотентна: ON CONFLICT DO NOTHING не трогает уже настроенные вручную
// правила. Не в org.CreateProject — чтобы не тянуть зависимость org → alert.
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
