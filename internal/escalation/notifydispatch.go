package escalation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// Не alert.Channel: alert уже импортирует notify, и приём alert.Channel
// напрямую здесь замкнул бы цикл alert -> escalation -> alert.
type DispatchChannel struct {
	ID int64
	// Kind — вид канала как есть (webhook/telegram/email) для payload
	// ("channel_kind", читает notify.Worker).
	Kind   string
	Target string
	// IsEmail — гейт email-fallback: email-канал пропускается, если
	// EmailEnabled=false в DispatchDeps.
	IsEmail bool
	// Deliverable — alert.Channel.Deliverable() вызывающего (включён и секрет
	// не сломан).
	Deliverable bool
	// Результат alert.DetailPolicy.AllowsDetails(ch) вызывающего: true — канал внутри
	// контура оператора, полный payload; иначе notify.RedactExternalPayload.
	AllowsDetails bool
}

// Duck-typed, не org.Service напрямую — его GetProject возвращает
// (org.Project, error), не (string, error). nil — уведомления идут без имени проекта.
type ProjectNamer interface {
	ProjectName(ctx context.Context, projectID int64) (string, error)
}

type OrgProjectNamer struct {
	Svc *org.Service
}

func (p OrgProjectNamer) ProjectName(ctx context.Context, projectID int64) (string, error) {
	if p.Svc == nil {
		return "", nil
	}
	proj, err := p.Svc.GetProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	return proj.Name, nil
}

// Интерфейс, не конкретный *notify.Outbox — тест золотого JSON вебхука
// прогоняет реальный Dispatch без похода в Postgres, подставив фейк.
type Enqueuer interface {
	Enqueue(ctx context.Context, channelID int64, payload map[string]any) error
	// EnqueueIdempotent — только при непустом DispatchInput.IdempotencyKeyPrefix;
	// enqueued=false — уже в очереди, не провал канала.
	EnqueueIdempotent(ctx context.Context, channelID int64, payload map[string]any, key string) (enqueued bool, err error)
}

type DispatchDeps struct {
	Outbox       Enqueuer
	EmailEnabled bool
	Projects     ProjectNamer
	// LogTag — префикс лог-сообщений и текста обёрнутых ошибок
	// ("host", "metric", "slo", "profile", "trace", "uptime", "alert").
	LogTag string
}

// Subject/Body уже локализованы вызывающим — у контура нет доменного знания
// форматов конкретного источника.
type DispatchInput struct {
	ProjectID int64
	// Kind — вид события для payload ("kind") и для redactedKindLabel на
	// обезличенном пути.
	Kind    string
	Subject string
	Body    string
	URL     string
	// RedactedURL — замена URL для канала без AllowsDetails, если сам адрес
	// несёт деталь (у host — имя машины в пути карточки хоста). "" — как URL.
	RedactedURL string
	// Extra — поля payload сверх маршрутного минимума: зона ответственности
	// каждого источника, контур сам их не строит.
	Extra map[string]any
	// ChannelIDs: nil/пусто — все deliverable-каналы Channels, непустой —
	// фильтр по членству ПОСЛЕ Deliverable-гейта (ContainsID).
	ChannelIDs []int64
	Channels   []DispatchChannel
	// IdempotencyKeyPrefix: непусто — ключ "<prefix>:<channelID>" через
	// EnqueueIdempotent, конфликт не провал. Пусто — обычный Enqueue.
	IdempotencyKeyPrefix string
}

// Возвращает ID каналов, в которые задача РЕАЛЬНО поставлена — логировать их
// в incident_escalations или нет, решает вызывающая оркестрация, не Dispatch.
func Dispatch(ctx context.Context, deps DispatchDeps, in DispatchInput) ([]int64, error) {
	subject, body := in.Subject, in.Body
	if name := resolveProjectName(ctx, deps, in.ProjectID); name != "" {
		subject = notify.WithProjectSubject(ctx, subject, name)
		body = notify.WithProjectBody(ctx, body, name)
		in.Extra = withProjectName(in.Extra, name)
	}

	var errs error
	var enqueued []int64
	for _, ch := range in.Channels {
		if !ch.Deliverable {
			continue
		}
		if len(in.ChannelIDs) > 0 && !ContainsID(in.ChannelIDs, ch.ID) {
			continue
		}
		if ch.IsEmail && !deps.EmailEnabled {
			slog.Warn(deps.LogTag+": notify: email channel skipped, SMTP not configured",
				"project_id", in.ProjectID, "channel_id", ch.ID)
			continue
		}
		payload := map[string]any{
			"kind":         in.Kind,
			"project_id":   in.ProjectID,
			"url":          in.URL,
			"subject":      subject,
			"body":         body,
			"channel_kind": ch.Kind,
			"target":       ch.Target,
			// Секрета в payload нет намеренно: notify.Worker достаёт его
			// по channel_id в момент отправки, иначе обесценил бы шифрование secret.
		}
		for k, v := range in.Extra {
			payload[k] = v
		}
		// Гейт трансграничной передачи: получателю вне контура оператора
		// уходит обезличенный payload (см. notify.RedactExternalPayload).
		if !ch.AllowsDetails {
			if in.RedactedURL != "" {
				payload["url_redacted"] = in.RedactedURL
			}
			payload = notify.RedactExternalPayload(ctx, payload)
		}
		if in.IdempotencyKeyPrefix != "" {
			key := fmt.Sprintf("%s:%d", in.IdempotencyKeyPrefix, ch.ID)
			if _, err := deps.Outbox.EnqueueIdempotent(ctx, ch.ID, payload, key); err != nil {
				slog.Error(deps.LogTag+": notify: enqueue idempotent failed", "channel_id", ch.ID, "key", key, "error", err)
				errs = errors.Join(errs, fmt.Errorf("%s: notify: enqueue channel %d: %w", deps.LogTag, ch.ID, err))
				continue
			}
			// enqueued=false здесь означает "уже стоит в очереди по этому
			// ключу" — канал всё равно обработан, не провалившийся.
			enqueued = append(enqueued, ch.ID)
			continue
		}
		if err := deps.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error(deps.LogTag+": notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("%s: notify: enqueue channel %d: %w", deps.LogTag, ch.ID, err))
			continue
		}
		enqueued = append(enqueued, ch.ID)
	}
	return enqueued, errs
}

// Best-effort: ошибка резолва не роняет уведомление целиком — деградирует
// до имени "", залогировав причину.
func resolveProjectName(ctx context.Context, deps DispatchDeps, projectID int64) string {
	if deps.Projects == nil {
		return ""
	}
	name, err := deps.Projects.ProjectName(ctx, projectID)
	if err != nil {
		slog.Warn(deps.LogTag+": notify: project name lookup failed", "project_id", projectID, "error", err)
		return ""
	}
	return name
}

// Копия, не мутация: extra в DispatchInput могла бы переиспользоваться
// вызывающим между вызовами Dispatch.
func withProjectName(extra map[string]any, name string) map[string]any {
	out := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		out[k] = v
	}
	out["project_name"] = name
	return out
}
