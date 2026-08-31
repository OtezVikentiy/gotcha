// Package alert — правила алертинга (new_issue/regression/spike) и каналы
// доставки (email/webhook/telegram) на уровне проекта.
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
	// ErrSecretBroken — секрет канала зашифрован (настоящий enc:-ciphertext),
	// но мастер-ключа нет (сброшен/откачен GOTCHA_SECRET_KEY): расшифровать
	// нечем, а отдавать ciphertext как живой секрет нельзя.
	ErrSecretBroken = errors.New("alert: channel secret is encrypted but no master key is set")
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
	// SecretBroken — секрет не расшифровывается (сменился или потерян
	// GOTCHA_SECRET_KEY). Канал при этом ОСТАЁТСЯ в списке: раньше он молча
	// выпадал из выдачи, и владелец не мог ни перевыпустить секрет, ни удалить
	// канал — обе ручки отвечали 404, потому что проверка принадлежности
	// строится поверх того же списка. При этом уведомления по нему просто
	// переставали ставиться в очередь: ни следа в журнале доставок, ни отметки
	// в интерфейсе. «Тишина в Telegram» была неотличима от «инцидентов не
	// было».
	//
	// Секрет у такого канала пустой — расшифровать его нечем.
	SecretBroken bool
	// Trusted — оператор заявил, что получатель этого канала внутри его
	// контура, и разрешил слать туда полные детали события.
	//
	// Нужен там, где получателя не опознать по адресу: у Telegram это
	// chat_id, домена у него нет, и политика оставляет такой канал внешним
	// всегда (см. DetailPolicy.AllowsDetails). На селфхосте, где оператор и
	// получатель — один человек, это оставляло единственный рычаг —
	// GOTCHA_EXTERNAL_CHANNEL_DETAILS, открывающий детали ВСЕМ каналам всех
	// проектов сразу. Выбор «нигде или везде» и есть причина этого поля:
	// то же решение, но поштучно.
	//
	// Дефолт — false, как и у всей политики: доверие возникает только явным
	// действием оператора в форме канала.
	Trusted bool
}

// Deliverable — можно ли слать в этот канал. Одно место на все семь
// нотифаеров: выключенный канал и канал со сломанным секретом одинаково не
// годятся для доставки, но по разным причинам, и разложенное по семи файлам
// правило разъехалось бы.
func (c Channel) Deliverable() bool { return c.Enabled && !c.SecretBroken }

// Service — CRUD над правилами и каналами алертинга.
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

// SetKeyring включает шифрование секретов каналов (Telegram bot-токен, HMAC-
// ключ webhook) at-rest тем же кольцом ключей, что и SSO client_secret. Не
// вызывается вовсе для dev-стендов — секреты остаются plaintext (Keyring.Open
// распознаёт это по отсутствию префикса "enc:"). Ставится из main.go.
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

// normalizeChannelTarget приводит получателя к тому виду, в котором он
// пригоден к использованию, и возвращает канал с исправленным Target.
//
// Для email это значит «только адрес, без отображаемого имени».
// mail.ParseAddress принимает и форму «Ops Team <ops@corp.example>», и раньше
// она сохранялась в БД как есть — со всеми последствиями: отправитель кладёт
// Target прямо в SMTP-команду RCPT TO, и сервер отвечает отказом, а политика
// раскрытия деталей видит домен «corp.example>» и не признаёт его своим. Обе
// поломки тихие: первая всплывает в журнале неудачных доставок уже после того,
// как алерт не пришёл, вторая не всплывает нигде.
func normalizeChannelTarget(c Channel) Channel {
	c.Target = strings.TrimSpace(c.Target)
	if c.Kind == ChannelEmail {
		if a, err := mail.ParseAddress(c.Target); err == nil {
			c.Target = a.Address
		}
	}
	return c
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
		if c.Secret == "" {
			return ErrInvalidChannel
		}
		// chat_id у Telegram — целое число (у групп и супергрупп
		// отрицательное). Раньше проверялась только непустота, и любая
		// опечатка — «не-урл», скопированное имя чата, пробел — принималась
		// молча, а узнать о ней было негде: доставка падала уже в фоне, в логе
		// неудачных отправок. Пусть форма ловит это сразу.
		if _, err := strconv.ParseInt(c.Target, 10, 64); err != nil {
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

// UpsertRules сохраняет НАБОР правил проекта атомарно: либо применяются все,
// либо ни одно.
//
// Раньше страница писала правила по очереди, и первая же ошибка обрывала цикл —
// уже записанные оставались. Пользователь получал 422, форма перерисовывалась из
// БД, и понять, что именно сохранилось, было нельзя: «нажал Сохранить, получил
// ошибку, а половина изменений всё-таки применилась».
//
// Валидация идёт целиком ДО первой записи, а сами записи — в одной транзакции:
// так частичное применение невозможно ни из-за невалидного правила, ни из-за
// сбоя БД посередине.
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

// DeleteRule удаляет правило по id.
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

// Channels возвращает каналы доставки проекта, отсортированные по id.
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
				// Деградируем ПОКАНАЛЬНО, а не роняем весь список: один
				// нерасшифруемый секрет (сменившийся или потерянный
				// GOTCHA_SECRET_KEY) не должен убивать доставку по остальным
				// каналам проекта.
				//
				// Но и выбрасывать канал из списка нельзя — так было раньше, и
				// это делало его невидимым дважды: доставка молча
				// прекращалась, а починить или удалить канал из интерфейса
				// было невозможно (проверка принадлежности строится поверх
				// этого же списка и отвечала 404). Возвращаем его помеченным:
				// Deliverable() отсечёт его у нотифаеров, а страница покажет,
				// что именно сломано.
				slog.Error("alert: channel secret cannot be decrypted",
					"channel_id", c.ID, "project_id", c.ProjectID, "kind", c.Kind, "error", err)
				c.Secret = ""
				c.SecretBroken = true
				out = append(out, c)
				continue
			}
			c.Secret = secret
		} else if secretbox.IsEncrypted(c.Secret) {
			// Мастер-ключа нет (dev-дефолт или откат GOTCHA_SECRET_KEY), но в
			// БД лежит НАСТОЯЩИЙ ciphertext, а не legacy plaintext. Отдать его
			// как есть — значит подсунуть нотифаеру enc:base64... вместо
			// bot-токена/HMAC-ключа: тихий отказ доставки вместо явного. Тот
			// же приём, что и выше при ошибке Open — помечаем сломанным и не
			// отдаём ciphertext.
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

// CreateChannel создаёт канал доставки проекта.
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

// ChannelSecret возвращает РАСШИФРОВАННЫЙ секрет канала по его id — bot-токен
// Telegram или HMAC-ключ вебхука.
//
// Существует ради notify.Worker: раньше секрет клали в payload задачи
// (notification_outbox.payload — обычный jsonb), и шифрование alert_channels
// .secret этим обесценивалось полностью — `SELECT payload->>'secret'` отдавал
// живые токены за всё окно хранения очереди. Теперь в очереди лежит только
// channel_id, а секрет достаётся здесь, в момент отправки, и живёт лишь в
// памяти воркера.
//
// ErrNotFound — канал удалили между постановкой в очередь и отправкой; это не
// сбой доставки, а исчезнувший адресат, и слать уже некуда.
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
			// Настоящий ciphertext без ключа для расшифровки — слать
			// нечего; отдать его как secret значило бы отправить
			// enc:base64... нотифаеру как bot-токен/HMAC-ключ.
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

// UpdateChannel меняет канал доставки: получателя, секрет и включённость.
//
// Раньше у канала был только жизненный цикл «создать/удалить»: выключенный
// Telegram-канал нельзя было включить из интерфейса, а опечатку в адресе —
// исправить. Приходилось удалять и заводить заново, теряя историю доставок.
//
// Пустой Secret означает «оставить прежний»: секрет вводится вслепую
// (type=password) и в форму не возвращается, поэтому требовать его при каждом
// изменении адреса значило бы заставлять оператора искать bot-токен ради
// правки опечатки.
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
	// project_id в условии — скоуп: id канала приходит из формы, и без него
	// владелец одного проекта мог бы править канал соседнего.
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

// validateChannelForUpdate — как validateChannel, но пустой секрет допустим:
// при изменении он означает «оставить прежний».
func validateChannelForUpdate(c Channel) error {
	probe := c
	if probe.Kind == ChannelTelegram && probe.Secret == "" {
		probe.Secret = "keep" // проверяем всё, кроме наличия секрета
	}
	return validateChannel(probe)
}

// DeleteChannel удаляет канал по id. Каскадом удаляет и его записи в outbox.
func (s *Service) DeleteChannel(ctx context.Context, projectID, channelID int64) error {
	// project_id в условии — тот же скоуп, что и у UpdateChannel. Хендлер и так
	// проверяет принадлежность перед удалением, но правило «id пришёл из формы,
	// значит скоуп в WHERE» должно жить в одном месте: разложенное по двум,
	// оно разъедется на первом вызывающем, который про предпроверку забудет.
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
