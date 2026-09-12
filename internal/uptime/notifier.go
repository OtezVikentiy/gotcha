package uptime

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

type depCounter interface {
	DeclaredChildrenCount(ctx context.Context, kind string, nodeID int64) (int, error)
}

type OutboxNotifier struct {
	Alerts *alert.Service // для Alerts.Channels(projectID) — фолбэк, если у монитора нет своих каналов
	Uptime *Service       // для Uptime.MonitorChannelIDs(monitorID)
	Outbox *notify.Outbox

	BaseURL string // префикс ссылки на монитор: {BaseURL}/monitors/{id}

	// пока false, email-каналы пропускаются (с warn-логом), чтобы не ставить
	// в очередь задачи, которые notify.Worker всё равно не доставит.
	EmailEnabled bool

	Details alert.DetailPolicy // нулевое значение не доверяет никому

	// локаль ИНСТАНСА, не запроса — внешний канал не знает языка получателя,
	// язык уведомления выбирает оператор.
	Locale i18n.Locale

	DepCounts depCounter // nil — строки «Зависимых узлов: N» не будет

	Projects escalation.ProjectNamer // nil — уведомления идут без имени проекта
}

// ошибка Enqueue по одному каналу не прерывает постановку остальных —
// собирается через errors.Join в возвращаемое значение.
func (n *OutboxNotifier) Notify(ctx context.Context, ev Event) error {
	_, err := n.dispatch(ctx, ev, nil, "")
	return err
}

// Резолвит, не отправляя — Detector клеймит результат в incident_escalations
// до вызова NotifyOpenStep0.
func (n *OutboxNotifier) OpenStep0Channels(ctx context.Context, ev Event) ([]int64, error) {
	channels, err := n.resolveChannels(ctx, ev.Monitor.ID, ev.Monitor.ProjectID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(channels))
	for i, ch := range channels {
		ids[i] = ch.ID
	}
	return ids, nil
}

// возвращает РЕАЛЬНО заенкененные каналы — Detector сам логирует их как шаг 0
// в incident_escalations, иначе recovery не найдёт адресатов.
func (n *OutboxNotifier) NotifyOpenStep0(ctx context.Context, ev Event, channelIDs []int64) ([]int64, error) {
	// "open" — шаг 0 uptime; другой шаг с этим же префиксом схлопнул бы ключи.
	return n.dispatch(ctx, ev, channelIDs, fmt.Sprintf("uptime:open:%d", ev.Incident.ID))
}

// инцидент/монитор грузятся заново по ID — вызывающий (Detector.resolveIncident)
// знает только incidentID, не готовый Event.
func (n *OutboxNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	inc, ok, err := n.Uptime.IncidentByID(ctx, incidentID)
	if err != nil {
		return fmt.Errorf("uptime: notify recovery: load incident: %w", err)
	}
	if !ok {
		return fmt.Errorf("uptime: notify recovery: incident %d not found", incidentID)
	}
	mon, err := n.Uptime.Get(ctx, inc.MonitorID)
	if err != nil {
		return fmt.Errorf("uptime: notify recovery: load monitor: %w", err)
	}
	var duration int64
	if inc.ResolvedAt != nil {
		duration = int64(inc.ResolvedAt.Sub(inc.StartedAt).Seconds())
	}
	ev := Event{Kind: "up", Monitor: mon, Incident: inc, DurationSeconds: duration}
	_, err = n.dispatch(ctx, ev, channelIDs, "")
	return err
}

// возвращает каналы, реально поставленные в очередь, не факт намерения.
// Только "down" — "up"/"ssl_expiring"/"reminder" идут вне лесенки эскалации.
func (n *OutboxNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	inc, ok, err := n.Uptime.IncidentByID(ctx, incidentID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify step: load incident: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("uptime: notify step: incident %d not found", incidentID)
	}
	mon, err := n.Uptime.Get(ctx, inc.MonitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify step: load monitor: %w", err)
	}
	ev := downEvent(mon, inc, inc.Regions, inc.Cause)
	return n.dispatch(ctx, ev, channelIDs, "")
}

// возвращает каналы, реально поставленные в очередь — вызывающие решают,
// логировать ли их.
// Общий для dispatch и OpenStep0Channels — оба обязаны видеть один список,
// иначе клейм и отправка расходятся.
func (n *OutboxNotifier) resolveChannels(ctx context.Context, monitorID, projectID int64) ([]alert.Channel, error) {
	own, err := n.Uptime.MonitorChannelIDs(ctx, monitorID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify: monitor channels: %w", err)
	}
	// тела каналов всегда берём у alert.Service — только он держит мастер-ключ
	// расшифровки и умеет пропустить канал с испорченным секретом.
	channels, err := n.Alerts.Channels(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("uptime: notify: project channels: %w", err)
	}
	if len(own) > 0 {
		// сужаем до привязанных каналов; если все отсеялись — молчим, не
		// откатываемся на каналы проекта, откуда оператор явно исключил монитор.
		want := make(map[int64]struct{}, len(own))
		for _, id := range own {
			want[id] = struct{}{}
		}
		filtered := make([]alert.Channel, 0, len(own))
		for _, ch := range channels {
			if _, ok := want[ch.ID]; ok {
				filtered = append(filtered, ch)
			}
		}
		channels = filtered
	}
	return channels, nil
}

func (n *OutboxNotifier) dispatch(ctx context.Context, ev Event, channelIDs []int64, idempotencyKeyPrefix string) ([]int64, error) {
	channels, err := n.resolveChannels(ctx, ev.Monitor.ID, ev.Monitor.ProjectID)
	if err != nil {
		return nil, err
	}

	ctx = i18n.WithLocale(ctx, n.Locale)

	url := fmt.Sprintf("%s/monitors/%d", n.BaseURL, ev.Monitor.ID)
	subject := subjectFor(ctx, ev)
	body := bodyFor(ctx, ev, url, n.depsLine(ctx, ev))

	dchans := make([]escalation.DispatchChannel, 0, len(channels))
	for _, ch := range channels {
		dchans = append(dchans, escalation.DispatchChannel{
			ID: ch.ID, Kind: ch.Kind, Target: ch.Target,
			IsEmail:       ch.Kind == alert.ChannelEmail,
			Deliverable:   ch.Deliverable(),
			AllowsDetails: n.Details.AllowsDetails(ch),
		})
	}

	return escalation.Dispatch(ctx,
		escalation.DispatchDeps{Outbox: n.Outbox, EmailEnabled: n.EmailEnabled, Projects: n.Projects, LogTag: "uptime"},
		escalation.DispatchInput{
			ProjectID: ev.Monitor.ProjectID, Kind: ev.Kind, Subject: subject, Body: body,
			URL: url,
			Extra: map[string]any{
				"monitor_id":       ev.Monitor.ID,
				"monitor_name":     ev.Monitor.Name,
				"regions":          ev.Regions,
				"cause":            ev.Cause,
				"duration_seconds": ev.DurationSeconds,
				"days_left":        ev.DaysLeft,
			},
			ChannelIDs: channelIDs, Channels: dchans,
			IdempotencyKeyPrefix: idempotencyKeyPrefix,
		})
}

// пусто при N=0, ошибке или отсутствии счётчика — уведомление не должно
// зависеть от доступности этого подсчёта.
func (n *OutboxNotifier) depsLine(ctx context.Context, ev Event) string {
	if n.DepCounts == nil || ev.Kind != "down" {
		return ""
	}
	cnt, err := n.DepCounts.DeclaredChildrenCount(ctx, "monitor", ev.Monitor.ID)
	if err != nil {
		slog.Warn("uptime: notify: declared children count failed", "monitor_id", ev.Monitor.ID, "error", err)
		return ""
	}
	if cnt == 0 {
		return ""
	}
	return i18n.Tf(ctx, "notify.uptime.deps_affected", "count", strconv.Itoa(cnt))
}

func subjectFor(ctx context.Context, ev Event) string {
	name := ev.Monitor.Name
	switch ev.Kind {
	case "down":
		return i18n.Tf(ctx, "notify.uptime.subject.down", "name", name)
	case "up":
		return i18n.Tf(ctx, "notify.uptime.subject.up",
			"name", name, "duration", formatDuration(ev.DurationSeconds))
	case "ssl_expiring":
		return i18n.Tf(ctx, "notify.uptime.subject.ssl",
			"name", name, "days", strconv.Itoa(ev.DaysLeft))
	case "reminder":
		return i18n.Tf(ctx, "notify.uptime.subject.reminder",
			"name", name, "duration", formatDuration(ev.DurationSeconds))
	default:
		return i18n.Tf(ctx, "notify.uptime.subject.generic", "name", name, "kind", ev.Kind)
	}
}

func bodyFor(ctx context.Context, ev Event, url, depsLine string) string {
	name := ev.Monitor.Name
	regions := strings.Join(ev.Regions, ", ")
	switch ev.Kind {
	case "down":
		return i18n.Tf(ctx, "notify.uptime.body.down",
			"name", name, "cause", ev.Cause, "regions", regions,
			"deps_line", depsLine, "url", url)
	case "up":
		return i18n.Tf(ctx, "notify.uptime.body.up",
			"name", name, "duration", formatDuration(ev.DurationSeconds), "url", url)
	case "ssl_expiring":
		return i18n.Tf(ctx, "notify.uptime.body.ssl",
			"name", name, "days", strconv.Itoa(ev.DaysLeft), "url", url)
	case "reminder":
		return i18n.Tf(ctx, "notify.uptime.body.reminder",
			"name", name, "duration", formatDuration(ev.DurationSeconds),
			"cause", ev.Cause, "regions", regions, "url", url)
	default:
		return i18n.Tf(ctx, "notify.uptime.body.generic", "name", name, "url", url)
	}
}

// компактный вид: "45s" (< 1 минуты), "2m5s" (< 1 часа), "1h5m" (>= 1 часа,
// секунды отбрасываются).
func formatDuration(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds) * time.Second
	h := int64(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int64(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int64(d / time.Second)

	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
