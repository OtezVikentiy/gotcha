package escalation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// DispatchChannel — маршрутные поля одного канала, нужные общему контуру
// Dispatch. Не alert.Channel: alert.Evaluator (issue-алерты, седьмой
// источник) сам зовёт Dispatch, и если бы этот пакет принимал alert.Channel
// напрямую, escalation пришлось бы импортировать alert — а alert уже
// импортирует notify, так что цикл замкнулся бы через
// alert -> escalation -> alert. Поэтому решения, требующие типов alert
// (Deliverable(), DetailPolicy.AllowsDetails(ch), ChannelEmail), вызывающий
// принимает САМ — обе точки уже единые (Channel.Deliverable и
// DetailPolicy.AllowsDetails, не то, что расходилось по семи файлам) — и
// передаёт сюда готовый минимум.
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
	// AllowsDetails — alert.DetailPolicy.AllowsDetails(ch) вызывающего: канал
	// внутри контура оператора получает полный payload, иначе —
	// notify.RedactExternalPayload.
	AllowsDetails bool
}

// ProjectNamer резолвит отображаемое имя проекта для уведомления (W3-E,
// кластер 4 «уведомления не называют проект»): duck-typed, а не org.Service
// напрямую — его GetProject возвращает (org.Project, error), не
// (string, error). Адаптер — OrgProjectNamer ниже. nil — уведомления идут
// без имени проекта, тот же nil-совместимый приём, что у depCounter/
// MaintenanceChecker в других пакетах этого репозитория.
type ProjectNamer interface {
	ProjectName(ctx context.Context, projectID int64) (string, error)
}

// OrgProjectNamer адаптирует *org.Service к ProjectNamer. Svc == nil —
// имени проекта нет (тесты, не заинтересованные в этой стороне поведения,
// его просто не заводят).
type OrgProjectNamer struct {
	Svc *org.Service
}

// ProjectName реализует ProjectNamer.
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

// Enqueuer — интерфейс постановки задачи в очередь, которого Dispatch
// требует от DispatchDeps.Outbox. *notify.Outbox реализует его штатно
// (Enqueue пишет в notification_outbox через pgx); в E3 (заморозка
// контракта вебхука) интерфейс понадобился тесту, замораживающему тело
// вебхука золотым JSON (internal/notify/webhook_golden_test.go) — он гонит
// РЕАЛЬНЫЙ Dispatch (резолв имени проекта, сборку Extra, редакцию ПДн) без
// похода в Postgres, подставляя вместо Outbox фейк, который просто
// запоминает payload.
type Enqueuer interface {
	Enqueue(ctx context.Context, channelID int64, payload map[string]any) error
}

// DispatchDeps — общие зависимости контура: собираются один раз при
// конструировании нотифаера (Alerts/Outbox/Details/EmailEnabled/Locale уже
// были такими полями до этой правки), не на каждый вызов.
type DispatchDeps struct {
	Outbox       Enqueuer
	EmailEnabled bool
	// Projects — источник имени проекта (nil-совместим, см. ProjectNamer).
	Projects ProjectNamer
	// LogTag — префикс лог-сообщений и текста обёрнутых ошибок ("host",
	// "metric", "slo", "profile", "trace", "uptime", "alert") — тот же
	// префикс, что раньше был захардкожен в каждой из семи копий.
	LogTag string
}

// DispatchInput — одно готовое к постановке уведомление. Subject/Body уже
// локализованы вызывающим: у контура нет доменного знания форматов
// конкретного источника, i18n остаётся его зоной ответственности.
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
	// Extra — поля payload сверх маршрутного минимума: остаётся зоной
	// ответственности каждого источника (значения метрик, имена целей и
	// т.п.), контур сам их не строит.
	Extra map[string]any
	// ChannelIDs — набор каналов ступени эскалации/recovery: nil/пусто — все
	// deliverable-каналы Channels, непустой — фильтр по членству ПОСЛЕ
	// Deliverable-гейта (ContainsID).
	ChannelIDs []int64
	Channels   []DispatchChannel
}

// Dispatch — единый контур доставки одного уведомления во ВСЕ подходящие
// каналы: гейт доставляемости, фильтр ступени/recovery (ContainsID),
// email-fallback, имя проекта в теме/теле/payload (notify.WithProjectSubject/
// WithProjectBody), редакция ПДн для внешних каналов
// (notify.RedactExternalPayload). Раньше был переписан по разу в каждом из
// семи источников (host/metric/slo/profile/trace/uptime/alert) и копии уже
// успели разойтись (W3-E: ContainsID был не во всех, адресность recovery — не
// у всех) — теперь один контур, семь вызывающих.
//
// Возвращает ID каналов, в которые задача РЕАЛЬНО поставлена — логировать их
// в incident_escalations или нет, решает вызывающая оркестрация
// (SendStepIfDue), не Dispatch (T7-fix: так было и раньше, у каждой из семи
// копий).
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
			// Секрета в payload нет намеренно: notification_outbox.payload —
			// обычный jsonb, и bot-токен/подпись в нём обесценили бы
			// шифрование alert_channels.secret. notify.Worker достаёт секрет
			// по channel_id в момент отправки (см. notify.SecretResolver).
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
		if err := deps.Outbox.Enqueue(ctx, ch.ID, payload); err != nil {
			slog.Error(deps.LogTag+": notify: enqueue failed", "channel_id", ch.ID, "error", err)
			errs = errors.Join(errs, fmt.Errorf("%s: notify: enqueue channel %d: %w", deps.LogTag, ch.ID, err))
			continue
		}
		enqueued = append(enqueued, ch.ID)
	}
	return enqueued, errs
}

// resolveProjectName — best-effort: ошибка резолва (проект успел исчезнуть
// между событием и доставкой, сбой БД) не должна ронять уведомление целиком
// — деградирует до имени "" (старое поведение, до W3-E), залогировав
// причину.
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

// withProjectName возвращает копию extra с добавленным "project_name" — не
// мутирует карту вызывающего: та же extra передаётся на каждый канал цикла
// Dispatch, и общий payload ниже и так копирует её поштучно, но сама extra
// в DispatchInput могла бы быть переиспользована вызывающим между вызовами.
func withProjectName(extra map[string]any, name string) map[string]any {
	out := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		out[k] = v
	}
	out["project_name"] = name
	return out
}
